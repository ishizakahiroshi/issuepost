// 読み出しの口。
//
//	GET /v1/cases            一覧（合言葉の範囲の中だけ）
//	GET /v1/cases/{number}   1 件
//
// **範囲は受け口の側で切る**（設計文書 §4）。合言葉ごとに読める範囲が決まっていて、
// その外は返さない。呼ぶ側の画面が絞ってくれることを当てにしない。
package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ishizakahiroshi/doorpost/config"
	"github.com/ishizakahiroshi/doorpost/gate"
	"github.com/ishizakahiroshi/doorpost/oplog"
	"github.com/ishizakahiroshi/doorpost/respond"

	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// errNotFound は番号に当たる案件が無い。
//
// **範囲の外にある番号も、これで返す。**「あるが見せない」と「そもそも無い」を
// 区別できると、番号を順に当てるだけで他の顧客の案件の有無が分かってしまう。
const errNotFound respond.Code = "not_found"

// caseItem は 1 件ぶんの応答。
//
// 日付（約束した期日・保留の解除期限）は YYYY-MM-DD で返す。時刻を持たない列なので、
// 時刻まで付けて返すと、受け取る側がタイムゾーンを当てはめて 1 日ずらす余地ができる。
// 日時（作成・更新・終端）は UTC のまま返す。
type caseItem struct {
	Number        string     `json:"number"`
	Source        string     `json:"source"`
	TenantRef     string     `json:"tenant_ref,omitempty"`
	Origin        string     `json:"origin"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	ApprovalState string     `json:"approval_state"`
	Title         string     `json:"title"`
	Body          string     `json:"body,omitempty"`
	ReporterRef   string     `json:"reporter_ref,omitempty"`
	Place         casePlace  `json:"place"`
	Fingerprint   string     `json:"fingerprint,omitempty"`
	PromisedDue   string     `json:"promised_due,omitempty"`
	HoldUntil     string     `json:"hold_until,omitempty"`
	ClosedAt      *time.Time `json:"closed_at,omitempty"`
	DuplicateOf   string     `json:"duplicate_of,omitempty"`
	LegacyRef     string     `json:"legacy_ref,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	People        int        `json:"people,omitempty"`
}

// caseList は一覧の応答。
//
// NextCursor が空でないときだけ続きがある。件数の総数は返さない。
// 総数を出すには範囲全体を数えることになり、一覧を開くたびに重い問い合わせが増える。
// 一覧が答えるのは「次に何を開くか」で、「全部で何件か」はパネルの仕事（§5）。
type caseList struct {
	Cases      []caseItem `json:"cases"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// filterFor は合言葉の範囲を、SQL に渡す絞り込みへ変える。
//
// "*" は「全部」なので条件を足さない。**それ以外は必ず足す。**
func filterFor(scope *Scope) store.Filter {
	var f store.Filter
	if !containsWildcard(scope.Source) {
		f.Sources = append(f.Sources, scope.Source...)
	}
	if !containsWildcard(scope.Tenant) {
		f.Tenants = append(f.Tenants, scope.Tenant...)
	}
	return f
}

func containsWildcard(values []string) bool {
	for _, v := range values {
		if v == Wildcard {
			return true
		}
	}
	return false
}

// authorize は鍵を見て、読める範囲を返す。返り値が nil なら応答は書き終えている。
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*config.App, *Scope) {
	app := gate.Authorize(r, s.cfg)
	if app == nil {
		s.logger.Print(oplog.Str("error", respond.InvalidKey))
		respond.Error(w, http.StatusUnauthorized, respond.InvalidKey)
		return nil, nil
	}
	scope := s.cfg.ScopeFor(app.Name)
	if scope == nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errOutOfScope))
		respond.Error(w, http.StatusForbidden, errOutOfScope)
		return nil, nil
	}
	return app, scope
}

// handleListCases は GET /v1/cases。
func (s *Server) handleListCases(w http.ResponseWriter, r *http.Request) {
	started := s.now()

	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	page, ok := decodePage(r)
	if !ok {
		respond.Error(w, http.StatusBadRequest, respond.InvalidRequest)
		return
	}

	cases, err := s.ledger.ListCases(r.Context(), filterFor(scope), page)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}

	out := caseList{Cases: make([]caseItem, 0, len(cases))}
	for _, c := range cases {
		out.Cases = append(out.Cases, toItem(c, false))
	}
	// 返した件数が上限と同じときだけ、続きの目印を付ける。
	// 少なければそこで終わりなので、空振りの 1 回を呼ぶ側にさせない。
	if len(cases) > 0 && len(cases) == page.Effective() {
		out.NextCursor = strconv.FormatUint(cases[len(cases)-1].ID, 10)
	}

	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "list"),
		oplog.Int("count", len(cases)),
		oplog.Dur("took", s.now().Sub(started)),
	)
	respond.JSON(w, http.StatusOK, out)
}

// handleGetCase は GET /v1/cases/{number}。
func (s *Server) handleGetCase(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}

	c, found, err := s.ledger.GetCase(r.Context(), filterFor(scope), number)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		// 範囲の外も、ここへ来る。理由を分けない。
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}
	respond.JSON(w, http.StatusOK, toItem(c, true))
}

// decodePage は ?limit= と ?cursor= を読む。
//
// 知らない問い合わせ文字列は無視せず弾く。書いても効かないより、そこで気づけるほうがよい
// （本文の知らないフィールドを拒否しているのと同じ考え方）。
func decodePage(r *http.Request) (store.Page, bool) {
	var p store.Page
	q := r.URL.Query()
	for key := range q {
		if key != "limit" && key != "cursor" {
			return p, false
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return p, false
		}
		p.Limit = n
	}
	if v := q.Get("cursor"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			return p, false
		}
		p.Cursor = n
	}
	// 上限そのものは store 側が持つ。ここで同じ数字を書くと、2 か所に同じ決まりが散る。
	return p, true
}

// toItem は返す形へ移す。full が false なら本文を落とす。
//
// **読み出す SQL が本文を選ばないことに頼らない。**頼ると、列を 1 つ足した日に
// 一覧の応答が本文の数だけ膨らむ。落とすことをこの層でも書いておく。
func toItem(c store.Case, full bool) caseItem {
	if !full {
		c.Body = ""
	}
	return caseItem{
		Number:        c.Number,
		Source:        c.Source,
		TenantRef:     c.TenantRef,
		Origin:        c.Origin,
		Kind:          c.Kind,
		Status:        c.Status,
		ApprovalState: c.ApprovalState,
		Title:         c.Title,
		Body:          c.Body,
		ReporterRef:   c.ReporterRef,
		Place: casePlace{
			ScreenID:    c.ScreenID,
			FeatureID:   c.FeatureID,
			Environment: c.Environment,
			Version:     c.Version,
			URL:         c.URL,
		},
		Fingerprint: c.Fingerprint,
		PromisedDue: formatDate(c.PromisedDue),
		HoldUntil:   formatDate(c.HoldUntil),
		ClosedAt:    c.ClosedAt,
		DuplicateOf: c.DuplicateOf,
		LegacyRef:   c.LegacyRef,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
		People:      c.People,
	}
}

func formatDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(dateLayout)
}

// splitCasePath は /v1/cases/ の後ろを分ける。
//
// 返すのは番号と、その下の名前（people / replies / links / attachments）と、
// さらにその下の識別子。無いところは空。
func splitCasePath(path string) (number, sub, id string, ok bool) {
	rest := strings.TrimPrefix(path, "/v1/cases/")
	if rest == "" || rest == path {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	for _, p := range parts {
		if p == "" {
			return "", "", "", false
		}
	}
	switch len(parts) {
	case 1:
		return parts[0], "", "", true
	case 2:
		return parts[0], parts[1], "", true
	case 3:
		return parts[0], parts[1], parts[2], true
	default:
		return "", "", "", false
	}
}

// handleCaseItem は /v1/cases/... を振り分ける。
//
// 知らない名前の下の口は、method を見ずに「無い」で返す。405 を返すと、
// その名前の口が在ることだけが分かる。
func (s *Server) handleCaseItem(w http.ResponseWriter, r *http.Request) {
	number, sub, id, ok := splitCasePath(r.URL.Path)
	if !ok {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.handleGetCase(w, r, number)
		case http.MethodPatch:
			s.handlePatchCase(w, r, number)
		default:
			respond.Error(w, http.StatusMethodNotAllowed, respond.InvalidRequest)
		}
	case subPeople, subReplies, subLinks:
		// この 3 つは下に識別子を持たない。/people/9 のような道は「無い」で返す。
		if id != "" {
			respond.Error(w, http.StatusNotFound, errNotFound)
			return
		}
		s.postOnly(w, r, func() {
			switch sub {
			case subPeople:
				s.handleAddPerson(w, r, number)
			case subReplies:
				s.handleAddReply(w, r, number)
			default:
				s.handleAddLink(w, r, number)
			}
		})
	case subAttachments:
		s.handleAttachments(w, r, number, id)
	default:
		respond.Error(w, http.StatusNotFound, errNotFound)
	}
}

func (s *Server) postOnly(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodPost {
		respond.Error(w, http.StatusMethodNotAllowed, respond.InvalidRequest)
		return
	}
	fn()
}
