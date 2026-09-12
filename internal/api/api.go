// Package api は台帳の受け口。
//
//	POST /v1/cases     案件を 1 件入れる。番号を返す
//	GET  /v1/health    生きているか
//
// **受け口は 1 本**（設計文書 §3）。人の報告も、アプリが検知した異常も同じ口で受ける。
// 出どころは送る項目（origin）であって、別の URL ではない。分けると取り決めが 2 本になり、
// 検査も 2 か所になり、片方だけ直して食い違う余地が常に残る。
//
// 読み出しの口は read.go、受け取ったあとの口（状態の変更・人・返事・関連）は edit.go、
// 添付は attach.go にある。合わせて設計文書 §3 の「口の一覧」になる。
//
// 鍵と IP の認証・応答の形・エラーの語彙・1 件 1 行のログは doorpost に任せる。
// 受け口を持つサービスならどれも同じになる部分で、台帳の仕事ではない。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/ishizakahiroshi/doorpost/config"
	"github.com/ishizakahiroshi/doorpost/gate"
	"github.com/ishizakahiroshi/doorpost/oplog"
	"github.com/ishizakahiroshi/doorpost/respond"

	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// 出どころ。設計が決めた値で、組織ごとに変わらない（スキーマ側も ENUM）。
const (
	originHuman    = "human"
	originDetected = "detected"
)

// 台帳だけのエラーの語。共通の語（invalid_request / invalid_key）は respond が持つ。
const (
	// errUnknownKind は種別が設定の一覧に無い。
	// invalid_request と分けるのは、送る側が直す場所が違うため
	// （本文の形ではなく、値そのもの）。
	errUnknownKind respond.Code = "unknown_kind"

	// errTitleTooLong は題名が長すぎる。**切らずに拒否する**（設計文書 §3）。
	// 切るのは加工で、黙って中身を変えるより送れなかったと伝えるほうがよい。
	errTitleTooLong respond.Code = "title_too_long"

	// errBodyTooLong は本文が保存できる長さを超えている。同じ理由で切らない。
	errBodyTooLong respond.Code = "body_too_long"

	// errUnknownStatus / errUnknownApproval は状態・承認が設定の一覧に無い。
	// errUnknownKind と同じ理由で invalid_request と分ける。
	errUnknownStatus   respond.Code = "unknown_status"
	errUnknownApproval respond.Code = "unknown_approval_state"

	// errHoldWithoutHoldState は、保留の解除期限を承認が保留でない案件へ付けようとした。
	// **期限だけが残ると、解除する相手のいない期限になる**（設計文書 §2）。
	errHoldWithoutHoldState respond.Code = "hold_without_hold_state"

	// errOutOfScope は合言葉の範囲の外。受け口はその外を書かないし返さない。
	errOutOfScope respond.Code = "out_of_scope"

	// errLedgerUnavailable は台帳へ保存できなかった。
	// **番号は返さない。**発行されていないので、呼ぶ側が番号を出せる経路が無い。
	errLedgerUnavailable respond.Code = "ledger_unavailable"
)

// 長さの上限は、すべてスキーマ（sql/0001_initial.sql）の列から来ている。
// ここで弾かないと、データベース側で黙って切られるか、保存そのものが落ちる。
// VARCHAR の上限は文字数なので、文字数で数える。
const (
	maxTitleRunes = 255      // cases.title VARCHAR(255)
	maxBodyBytes  = 16777215 // cases.body MEDIUMTEXT
	maxRefRunes   = 128      // reporter_ref / tenant_ref / legacy_ref / screen_id / feature_id
	maxShortRunes = 64       // environment / version
	maxURLRunes   = 1024     // cases.url
	fingerprintLn = 64       // cases.fingerprint CHAR(64)

	maxNoteRunes    = 1024 // case_events.note VARCHAR(1024)
	maxLinkRefRunes = 512  // case_links.ref VARCHAR(512)
	maxFileRunes    = 255  // case_attachments.filename VARCHAR(255)
	maxMimeRunes    = 127  // case_attachments.mime VARCHAR(127)

	// maxRequestBytes は 1 回の要求の上限。本文の上限に、他の項目のぶんの余白を足す。
	maxRequestBytes = maxBodyBytes + (1 << 20)

	// dateLayout は promised_due の形。日付だけで、時刻もタイムゾーンも持たない。
	dateLayout = "2006-01-02"
)

// Ledger は案件を保存する先。テストで差し替えられるように口だけを取る。
//
// **範囲（[store.Filter]）を取らない口は CreateCase だけ。**入れるときの範囲は
// 合言葉が source を固定することで決まり、それ以外の口は番号から入るので、
// 範囲を渡さない形にすると範囲の外へ届いてしまう。
type Ledger interface {
	CreateCase(ctx context.Context, c store.NewCase) (store.Created, error)
	ListCases(ctx context.Context, f store.Filter, p store.Page) ([]store.Case, error)
	GetCase(ctx context.Context, f store.Filter, number string) (store.Case, bool, error)
	UpdateCase(ctx context.Context, f store.Filter, number string, ch store.Changes) (store.Case, bool, error)
	AddPerson(ctx context.Context, f store.Filter, number, reporterRef string) (added, found bool, err error)
	AddReply(ctx context.Context, f store.Filter, number, body, authorRef string) (id uint64, found bool, err error)
	AddLink(ctx context.Context, f store.Filter, number, linkType, ref string) (id uint64, added, found bool, err error)
	AddAttachment(ctx context.Context, f store.Filter, number string, a store.NewAttachment) (id uint64, found bool, err error)
	FindAttachment(ctx context.Context, f store.Filter, number string, id uint64) (store.Attachment, bool, error)
	MarkAttachmentDeleted(ctx context.Context, id uint64, reason string) error
	UsedBytes(ctx context.Context) (uint64, error)
}

// Server は台帳の HTTP の口。
type Server struct {
	cfg    *Config
	ledger Ledger
	files  Files
	logger *oplog.Logger
	now    func() time.Time
}

// New は口を作る。
//
// files は添付の実体の置き場。**nil を渡してよい。**置き場が設定に無い構成では
// 添付の口だけが動かず、案件はそのまま受け付ける（設計文書 §6）。
func New(cfg *Config, ledger Ledger, files Files, logger *oplog.Logger) *Server {
	if logger == nil {
		logger = oplog.Wrap(nil)
	}
	return &Server{cfg: cfg, ledger: ledger, files: files, logger: logger, now: time.Now}
}

// Handler はルーティング済みのハンドラを返す。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/cases", s.handleCases)
	mux.HandleFunc("/v1/cases/", s.handleCaseItem)
	mux.HandleFunc("/v1/health", s.handleHealth)
	return mux
}

// handleHealth は生きているかだけを返す。中身も件数も返さない。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		respond.Error(w, http.StatusMethodNotAllowed, respond.InvalidRequest)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// caseRequest は POST /v1/cases の本文。
//
// source は受け取らない。合言葉から決まる（設計文書 §4）。本文に書かせると、
// どのアプリでも他のアプリの接頭辞で採番できてしまう。
// number も受け取らない。発行するのは台帳だけ（§3）。
//
// 知らないフィールドは拒否する。書いても黙って無視されるより、そこで気づけるほうがよい。
type caseRequest struct {
	Origin      string    `json:"origin"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	ReporterRef string    `json:"reporter_ref"`
	TenantRef   string    `json:"tenant_ref"`
	Place       casePlace `json:"place"`
	Fingerprint string    `json:"fingerprint"`
	LegacyRef   string    `json:"legacy_ref"`
	PromisedDue string    `json:"promised_due"`
}

// casePlace は発生場所。アプリが付けるもので、報告者に選ばせない。
type casePlace struct {
	ScreenID    string `json:"screen_id"`
	FeatureID   string `json:"feature_id"`
	Environment string `json:"environment"`
	Version     string `json:"version"`
	URL         string `json:"url"`
}

// caseResponse は受け取ったときに返すもの。台帳が発行した番号と、いまの状態。
type caseResponse struct {
	Number string `json:"number"`
	Status string `json:"status"`
}

func (s *Server) handleCases(w http.ResponseWriter, r *http.Request) {
	started := s.now()

	if r.Method == http.MethodGet {
		s.handleListCases(w, r)
		return
	}
	if r.Method != http.MethodPost {
		respond.Error(w, http.StatusMethodNotAllowed, respond.InvalidRequest)
		return
	}

	// 鍵と呼び出し元 IP を先に見る。落ちた理由はどちらも同じ 401 invalid_key にして、
	// 「鍵は合っていたが IP で落ちた」と読み取れないようにする。
	app := gate.Authorize(r, s.cfg)
	if app == nil {
		s.logger.Print(oplog.Str("error", respond.InvalidKey))
		respond.Error(w, http.StatusUnauthorized, respond.InvalidKey)
		return
	}
	scope := s.cfg.ScopeFor(app.Name)
	if scope == nil {
		// 検証を通った設定では起こらない。起きたなら設定の読み込みが壊れている。
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errOutOfScope))
		respond.Error(w, http.StatusForbidden, errOutOfScope)
		return
	}

	newCase, status, code := s.decodeCase(r, app, scope)
	if code != "" {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", code))
		respond.Error(w, status, code)
		return
	}

	created, err := s.ledger.CreateCase(r.Context(), newCase)
	if err != nil {
		// 保存できていないので番号は出さない。預からないし、送り直しもしない。
		// 呼ぶ側は失敗として扱い、書いた文を画面に残す（設計文書 §3）。
		s.logger.Print(
			oplog.Str("app", app.Name),
			oplog.Str("source", newCase.Source),
			oplog.Str("error", errLedgerUnavailable),
		)
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}

	// 受付 1 件につき 1 行。**題名・本文・報告者・URL は書かない。**
	// 運用ログは人が読む前提で残り続けるので、1 行に入れた個人の情報は
	// 消す口が無いまま溜まっていく。
	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("source", newCase.Source),
		oplog.Str("tenant", newCase.TenantRef),
		oplog.Str("origin", newCase.Origin),
		oplog.Str("kind", newCase.Kind),
		oplog.Str("number", created.Number),
		oplog.Bool("duplicate", created.Existed),
		oplog.Dur("took", s.now().Sub(started)),
	)

	// 既にあったものを返すときは 200、新しく作ったときは 201。
	// どちらも同じ番号を返す（同じものを 2 回送っても 2 件にならない・§3）。
	httpStatus := http.StatusCreated
	if created.Existed {
		httpStatus = http.StatusOK
	}
	respond.JSON(w, httpStatus, caseResponse{Number: created.Number, Status: created.Status})
}

// decodeCase は本文を読んで検証し、保存する形にする。code が空なら成功。
func (s *Server) decodeCase(r *http.Request, app *config.App, scope *Scope) (store.NewCase, int, respond.Code) {
	var req caseRequest
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return store.NewCase{}, http.StatusRequestEntityTooLarge, errBodyTooLong
		}
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}

	// どの合言葉で入れるか。範囲が 1 つの source に固定されていないと決まらない。
	source, ok := scope.IntakeSource()
	if !ok {
		return store.NewCase{}, http.StatusForbidden, errOutOfScope
	}

	if utf8.RuneCountInString(req.TenantRef) > maxRefRunes {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	tenant, ok := resolveTenant(scope, req.TenantRef)
	if !ok {
		if req.TenantRef == "" {
			// 範囲に顧客が複数あるのに本文が書いていない。どれか決められない。
			return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
		}
		return store.NewCase{}, http.StatusForbidden, errOutOfScope
	}

	if req.Origin != originHuman && req.Origin != originDetected {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	if !s.cfg.Kinds.Has(req.Kind) {
		return store.NewCase{}, http.StatusBadRequest, errUnknownKind
	}

	if req.Title == "" {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	if utf8.RuneCountInString(req.Title) > maxTitleRunes {
		return store.NewCase{}, http.StatusBadRequest, errTitleTooLong
	}
	if req.Body == "" {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	if len(req.Body) > maxBodyBytes {
		return store.NewCase{}, http.StatusRequestEntityTooLarge, errBodyTooLong
	}

	// 検知した異常には指紋があり報告者がいない。人の報告はその逆。
	// スキーマ側も CHECK で同じことを縛っているので、ここで弾かないと保存で落ちる。
	switch req.Origin {
	case originHuman:
		if req.ReporterRef == "" || utf8.RuneCountInString(req.ReporterRef) > maxRefRunes {
			return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
		}
		if req.Fingerprint != "" {
			return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
		}
	case originDetected:
		if req.ReporterRef != "" {
			return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
		}
		if !isFingerprint(req.Fingerprint) {
			return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
		}
	}

	if utf8.RuneCountInString(req.Place.ScreenID) > maxRefRunes ||
		utf8.RuneCountInString(req.Place.FeatureID) > maxRefRunes ||
		utf8.RuneCountInString(req.Place.Environment) > maxShortRunes ||
		utf8.RuneCountInString(req.Place.Version) > maxShortRunes ||
		utf8.RuneCountInString(req.Place.URL) > maxURLRunes {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	// 環境名は設定に登録されたものだけを受ける。
	// 書かないことは許すが、書くなら合言葉が名乗れる環境でなければならない。
	if req.Place.Environment != "" && !app.AllowsEnv(req.Place.Environment) {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}

	if utf8.RuneCountInString(req.LegacyRef) > maxRefRunes {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}
	if req.PromisedDue != "" && !isDate(req.PromisedDue) {
		return store.NewCase{}, http.StatusBadRequest, respond.InvalidRequest
	}

	return store.NewCase{
		Source:        source,
		TenantRef:     tenant,
		Origin:        req.Origin,
		Kind:          req.Kind,
		Status:        s.cfg.Statuses.Initial,
		ApprovalState: s.cfg.Approvals.For(s.cfg.Kinds.NeedsApproval(req.Kind)),
		Title:         req.Title,
		Body:          req.Body,
		ReporterRef:   req.ReporterRef,
		ScreenID:      req.Place.ScreenID,
		FeatureID:     req.Place.FeatureID,
		Environment:   req.Place.Environment,
		Version:       req.Place.Version,
		URL:           req.Place.URL,
		Fingerprint:   req.Fingerprint,
		PromisedDue:   req.PromisedDue,
		LegacyRef:     req.LegacyRef,
	}, 0, ""
}

// resolveTenant は保存する顧客の識別子を決める。
//
// 本文が書いていれば範囲の中かを見て、書いていなければ範囲から決める。
// **呼ぶ側の画面が絞ってくれることを当てにしない**（設計文書 §4）。
func resolveTenant(scope *Scope, given string) (string, bool) {
	if given != "" {
		if !scope.AllowsTenant(given) {
			return "", false
		}
		return given, true
	}
	return scope.DefaultTenant()
}

// isFingerprint は指紋が 64 桁の小文字 16 進かを見る。
// スキーマが CHAR(64) の ascii_bin なので、形が違うものは保存できない。
func isFingerprint(v string) bool {
	if len(v) != fingerprintLn {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}
