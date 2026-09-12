// 受け取ったあとの口。
//
//	PATCH /v1/cases/{number}           状態・承認・期限 2 本を変える
//	POST  /v1/cases/{number}/people    同じことに当たった人を 1 人足す
//	POST  /v1/cases/{number}/replies   返事を足す
//	POST  /v1/cases/{number}/links     コミット・文書・URL を足す
//
// **案件は入れて終わりではない**（設計文書 §3）。状態が変わり、承認が下り、返事を返し、
// 証拠がぶら下がる。口が無ければ、4 本のアプリがそれぞれ違う形で用意することになる。
//
// **どの口も、範囲の外の番号は「無い」と同じに返す。**書けるかどうかで存在が漏れない。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/ishizakahiroshi/doorpost/oplog"
	"github.com/ishizakahiroshi/doorpost/respond"

	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// 関連の種類。設計が決めた値で、組織ごとに変わらない（スキーマ側も ENUM）。
const (
	linkCommit = "commit"
	linkDoc    = "doc"
	linkURL    = "url"
)

// 下にぶら下がる口の名前。
const (
	subPeople      = "people"
	subReplies     = "replies"
	subLinks       = "links"
	subAttachments = "attachments"
)

// nullable は「書かれなかった」「値が来た」「空にする（null）」の 3 つを区別する。
//
// **区別できないと、保留を解除する操作が書けない。**期限を空へ戻すことと、
// 期限に触らないことが同じ形になり、どちらの意味でも読めてしまう。
type nullable struct {
	set   bool
	null  bool
	value string
}

// UnmarshalJSON は null でも呼ばれる（encoding/json は Unmarshaler を持つ型に対し、
// 入力が null のときも呼ぶと決めてある）。だから 3 つを区別できる。
func (n *nullable) UnmarshalJSON(b []byte) error {
	n.set = true
	if string(b) == "null" {
		n.null = true
		n.value = ""
		return nil
	}
	return json.Unmarshal(b, &n.value)
}

// text は値を指す。書かれていなければ nil。null なら空文字を指す。
func (n nullable) text() *string {
	if !n.set {
		return nil
	}
	v := n.value
	return &v
}

// patchRequest は PATCH /v1/cases/{number} の本文。
//
// 番号・出どころ・題名・本文は変えられない。**台帳は受け取ったものを書き換えない**
// （原則 4）。変えられるのは、受け取ったあとに動くものだけ。
type patchRequest struct {
	Status        nullable `json:"status"`
	ApprovalState nullable `json:"approval_state"`
	PromisedDue   nullable `json:"promised_due"`
	HoldUntil     nullable `json:"hold_until"`

	// ActorRef は変えた人。自動で変わるもの（期限切れの繰り上げなど）では空でよい。
	ActorRef string `json:"actor_ref"`
	// Note は理由。却下の理由などが入り、この変更で残る経緯の行すべてに付く。
	Note string `json:"note"`
}

// handlePatchCase は PATCH /v1/cases/{number}。
func (s *Server) handlePatchCase(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	ch, status, code := s.decodeChanges(r)
	if code != "" {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", code))
		respond.Error(w, status, code)
		return
	}

	got, found, err := s.ledger.UpdateCase(r.Context(), filterFor(scope), number, ch)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "patch"),
		oplog.Str("number", got.Number),
		oplog.Str("status", got.Status),
		oplog.Str("approval", got.ApprovalState),
	)
	respond.JSON(w, http.StatusOK, toItem(got, true))
}

// decodeChanges は本文を読んで検証し、変更の形にする。code が空なら成功。
//
// **値は設定の一覧に対して検査する**（設計文書 §2）。一覧に無い状態を受けると、
// 誰待ちの集計にも、終端かどうかの判定にも入らない行ができる。
func (s *Server) decodeChanges(r *http.Request) (store.Changes, int, respond.Code) {
	var req patchRequest
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return store.Changes{}, http.StatusRequestEntityTooLarge, errBodyTooLong
		}
		return store.Changes{}, http.StatusBadRequest, respond.InvalidRequest
	}

	// 状態と承認は空にできない（列が NOT NULL）。null を受けたらそこで弾く。
	if req.Status.null || req.ApprovalState.null {
		return store.Changes{}, http.StatusBadRequest, respond.InvalidRequest
	}
	if req.Status.set && !s.cfg.Statuses.Has(req.Status.value) {
		return store.Changes{}, http.StatusBadRequest, errUnknownStatus
	}
	if req.ApprovalState.set && !s.cfg.Approvals.Has(req.ApprovalState.value) {
		return store.Changes{}, http.StatusBadRequest, errUnknownApproval
	}

	for _, d := range []nullable{req.PromisedDue, req.HoldUntil} {
		if d.set && !d.null && !isDate(d.value) {
			return store.Changes{}, http.StatusBadRequest, respond.InvalidRequest
		}
	}
	// 保留の解除期限は、承認が保留のときだけ持てる。
	// **同じ要求で承認を保留にするなら通す。**2 回に分けさせると、
	// 1 回目と 2 回目の間に期限の無い保留ができる。
	if req.HoldUntil.set && !req.HoldUntil.null {
		if !req.ApprovalState.set || req.ApprovalState.value != s.cfg.Approvals.Hold {
			return store.Changes{}, http.StatusBadRequest, errHoldWithoutHoldState
		}
	}

	if utf8.RuneCountInString(req.ActorRef) > maxRefRunes ||
		utf8.RuneCountInString(req.Note) > maxNoteRunes {
		return store.Changes{}, http.StatusBadRequest, respond.InvalidRequest
	}

	ch := store.Changes{
		Status:        req.Status.text(),
		ApprovalState: req.ApprovalState.text(),
		PromisedDue:   req.PromisedDue.text(),
		HoldUntil:     req.HoldUntil.text(),
		ActorRef:      req.ActorRef,
		Note:          req.Note,
	}
	// 終端かどうかを決めるのは設定なので、判定はここで済ませて渡す。
	// closed_at の出し入れは台帳の仕事で、どの語が終端かは台帳の知るところではない。
	if req.Status.set {
		terminal := s.cfg.Statuses.IsTerminal(req.Status.value)
		ch.Terminal = &terminal
	}
	if ch.Empty() {
		// 変えるものが 1 つも無い。受けると、何も起きないのに 200 が返る。
		return store.Changes{}, http.StatusBadRequest, respond.InvalidRequest
	}
	return ch, 0, ""
}

// personRequest は POST /v1/cases/{number}/people の本文。
type personRequest struct {
	ReporterRef string `json:"reporter_ref"`
}

// handleAddPerson は「同じことに当たった人」を 1 人足す。
//
// **2 件目以降の報告は、新しい案件を作らずここへ送る**（設計文書 §3）。
// この件数が優先順位の根拠になる。
func (s *Server) handleAddPerson(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	var req personRequest
	if !decodeBody(r, &req) {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}
	if req.ReporterRef == "" || utf8.RuneCountInString(req.ReporterRef) > maxRefRunes {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}

	added, found, err := s.ledger.AddPerson(r.Context(), filterFor(scope), number, req.ReporterRef)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "people"),
		oplog.Str("number", number),
		oplog.Bool("added", added),
	)
	// 既に数えられていた人なら 200。どちらも「その人は数えられている」で変わらない。
	respond.JSON(w, createdOr200(added), map[string]bool{"added": added})
}

// replyRequest は POST /v1/cases/{number}/replies の本文。
type replyRequest struct {
	Body      string `json:"body"`
	AuthorRef string `json:"author_ref"`
}

// handleAddReply は返事を 1 件足す。
func (s *Server) handleAddReply(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	var req replyRequest
	if !decodeBody(r, &req) {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}
	if req.Body == "" {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}
	if len(req.Body) > maxBodyBytes {
		respond.Error(w, http.StatusRequestEntityTooLarge, errBodyTooLong)
		return
	}
	if req.AuthorRef == "" || utf8.RuneCountInString(req.AuthorRef) > maxRefRunes {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}

	id, found, err := s.ledger.AddReply(r.Context(), filterFor(scope), number, req.Body, req.AuthorRef)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	// **本文は書かない。**返事は報告した人へ向けた文で、運用ログに残す理由が無い。
	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "reply"),
		oplog.Str("number", number),
	)
	respond.JSON(w, http.StatusCreated, map[string]uint64{"id": id})
}

// linkRequest は POST /v1/cases/{number}/links の本文。
type linkRequest struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// handleAddLink はコミット・文書・URL を 1 つ足す。
//
// **同じものを 2 回送っても 1 行**（設計文書 §3）。ビルドの仕組みが再実行されるたびに
// 同じコミットが積み上がると、直した証拠が読めなくなる。
func (s *Server) handleAddLink(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	var req linkRequest
	if !decodeBody(r, &req) {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}
	switch req.Type {
	case linkCommit, linkDoc, linkURL:
	default:
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}
	if req.Ref == "" || utf8.RuneCountInString(req.Ref) > maxLinkRefRunes {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}

	id, added, found, err := s.ledger.AddLink(r.Context(), filterFor(scope), number, req.Type, req.Ref)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "link"),
		oplog.Str("number", number),
		oplog.Str("type", req.Type),
		oplog.Bool("added", added),
	)
	respond.JSON(w, createdOr200(added), map[string]any{"id": strconv.FormatUint(id, 10), "added": added})
}

// decodeBody は小さい本文を読む。知らないフィールドは拒否する。
func decodeBody(r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(into) == nil
}

// createdOr200 は、新しく作ったなら 201、既にあったなら 200。
func createdOr200(added bool) int {
	if added {
		return http.StatusCreated
	}
	return http.StatusOK
}

// isDate は YYYY-MM-DD かを見る。時刻もタイムゾーンも受けない。
func isDate(v string) bool {
	_, err := time.ParseInLocation(dateLayout, v, time.UTC)
	return err == nil
}
