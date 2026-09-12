// 読み出し。一覧と 1 件。
//
// **範囲の絞り込みはここで必ず SQL に入る**（設計文書 §4）。呼ぶ側の画面が絞ってくれる
// ことを当てにしない。画面を作る人が条件を 1 か所書き忘れると、ある顧客に別の顧客の
// 案件がそのまま出る。それは、やってしまってから気づく種類の間違いなので、
// 「絞り忘れた画面はただ何も余計に受け取らない」形にしておく。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// Filter は読める範囲。合言葉から作る。
//
// **空のスライスは「全部」を表す**（合言葉の範囲が "*" のとき）。
// 運営側の横断画面だけがこの形になり、顧客側の合言葉は必ず 1 つ以上入る。
type Filter struct {
	Sources []string
	Tenants []string
}

// Page は切り出し方。id の降順に、cursor より小さいものを Limit 件まで。
//
// 件数を飛ばす形（OFFSET）にしないのは、読んでいる間に新しい案件が入ると
// 境目がずれて、同じ行が 2 回出たり 1 回も出なかったりするため。
// id を鍵にすれば、途中で増えても既に読んだところは動かない。
type Page struct {
	Limit  int
	Cursor uint64 // 0 なら先頭から
}

// Effective は実際に使われる件数の上限。呼ぶ側が「続きがあるか」を
// 判定するのに要る。上限の数字はこの層だけが持つ。
func (p Page) Effective() int { return p.limit() }

func (p Page) limit() int {
	switch {
	case p.Limit <= 0:
		return defaultPageLimit
	case p.Limit > maxPageLimit:
		return maxPageLimit
	default:
		return p.Limit
	}
}

// Case は読み出した案件 1 件。
//
// Body は 1 件を引いたときだけ入る。一覧に本文を載せると、
// 1 回の応答が本文の数だけ膨らむ。一覧が欲しいのは「どれを開くか」を決める材料で、
// 中身そのものではない。
type Case struct {
	// ID は次の切り出し（cursor）に使う内部の番号。
	// **応答には出さない。**外へ出すのは番号（[Case.Number]）だけで、
	// 連番の内部 ID を渡すと、足し引きするだけで隣の案件の有無が分かる。
	ID uint64

	Number        string
	Source        string
	TenantRef     string
	Origin        string
	Kind          string
	Status        string
	ApprovalState string
	Title         string
	Body          string
	ReporterRef   string
	ScreenID      string
	FeatureID     string
	Environment   string
	Version       string
	URL           string
	Fingerprint   string
	PromisedDue   *time.Time
	HoldUntil     *time.Time
	ClosedAt      *time.Time
	DuplicateOf   string // まとめた先の番号。無ければ空
	LegacyRef     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	People        int // 同じことに当たった人の数
}

// where は範囲を SQL の条件にする。列名には別名 c を付ける前提。
//
// 全部が読める範囲（"*"）のときだけ条件を足さない。**それ以外は必ず足す。**
func (f Filter) where() (string, []any) {
	var conds []string
	var args []any
	if len(f.Sources) > 0 {
		conds = append(conds, "c.source IN ("+placeholders(len(f.Sources))+")")
		for _, v := range f.Sources {
			args = append(args, v)
		}
	}
	if len(f.Tenants) > 0 {
		conds = append(conds, "c.tenant_ref IN ("+placeholders(len(f.Tenants))+")")
		for _, v := range f.Tenants {
			args = append(args, v)
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(conds, " AND "), args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// 一覧で返す列。本文は入れない。
const listColumns = `
  c.id, c.number, c.source, c.tenant_ref, c.origin, c.kind, c.status, c.approval_state,
  c.title, c.reporter_ref, c.screen_id, c.feature_id, c.environment, c.version, c.url,
  c.fingerprint, c.promised_due, c.hold_until, c.closed_at, c.legacy_ref,
  c.created_at, c.updated_at`

// ListCases は範囲の中の案件を、新しいものから順に返す。
//
// 返る件数が Limit と同じなら、続きがある可能性がある。続きは最後の [Case.ID] を
// Page.Cursor に渡して引く。
func (s *Store) ListCases(ctx context.Context, f Filter, p Page) ([]Case, error) {
	scope, args := f.where()

	q := "SELECT" + listColumns + " FROM cases c WHERE 1=1" + scope
	if p.Cursor > 0 {
		q += " AND c.id < ?"
		args = append(args, p.Cursor)
	}
	q += " ORDER BY c.id DESC LIMIT ?"
	args = append(args, p.limit())

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 案件の一覧を引けない")
	}
	defer func() { _ = rows.Close() }()

	var out []Case
	for rows.Next() {
		c, err := scanList(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 案件の一覧を読み切れない")
	}
	return out, nil
}

// 1 件で返す列。本文と、まとめた先の番号と、当たった人の数が増える。
const itemColumns = listColumns + `,
  c.body,
  d.number AS duplicate_number,
  (SELECT COUNT(*) FROM case_people p WHERE p.case_id = c.id) AS people_count`

// rowQuerier は 1 行を引く口。*sql.DB と *sql.Tx のどちらでも渡せる。
//
// **番号から案件を引くところは口 8 本すべてで要る。**トランザクションの中と外の
// 両方から同じ 1 本を呼べるように、受け口を狭くしておく。
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// caseIDFor は番号から台帳の中の id を引く。
//
// **範囲の外にある番号は「無い」と同じに返す**（[Store.GetCase] と同じ理由）。
// 案件にぶら下げる口（人・返答・つながり・添付）は、すべてここを通ってから書く。
func caseIDFor(ctx context.Context, q rowQuerier, f Filter, number string) (uint64, bool, error) {
	scope, args := f.where()
	args = append([]any{number}, args...)

	var id uint64
	err := q.QueryRowContext(ctx, "SELECT c.id FROM cases c WHERE c.number = ?"+scope, args...).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("store: 案件を引けない")
	}
	return id, true, nil
}

// GetCase は番号で 1 件引く。
//
// **範囲の外にある番号は「無い」と同じに返す。**「あるが見せない」と
// 「そもそも無い」を区別できると、番号を順に当てるだけで他の顧客に
// 案件が何件あるかが分かってしまう。
func (s *Store) GetCase(ctx context.Context, f Filter, number string) (Case, bool, error) {
	return getCase(ctx, s.db, f, number)
}

func getCase(ctx context.Context, q rowQuerier, f Filter, number string) (Case, bool, error) {
	scope, args := f.where()

	sel := "SELECT" + itemColumns +
		" FROM cases c LEFT JOIN cases d ON d.id = c.duplicate_of" +
		" WHERE c.number = ?" + scope
	args = append([]any{number}, args...)

	var (
		c        Case
		tenant   sql.NullString
		reporter sql.NullString
		finger   sql.NullString
		due      sql.NullTime
		hold     sql.NullTime
		closed   sql.NullTime
		legacy   sql.NullString
		dupNum   sql.NullString
	)
	err := q.QueryRowContext(ctx, sel, args...).Scan(
		&c.ID, &c.Number, &c.Source, &tenant, &c.Origin, &c.Kind, &c.Status, &c.ApprovalState,
		&c.Title, &reporter, &c.ScreenID, &c.FeatureID, &c.Environment, &c.Version, &c.URL,
		&finger, &due, &hold, &closed, &legacy,
		&c.CreatedAt, &c.UpdatedAt,
		&c.Body, &dupNum, &c.People,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Case{}, false, nil
	case err != nil:
		return Case{}, false, fmt.Errorf("store: 案件を引けない")
	}

	c.TenantRef = tenant.String
	c.ReporterRef = reporter.String
	c.Fingerprint = finger.String
	c.LegacyRef = legacy.String
	c.DuplicateOf = dupNum.String
	c.PromisedDue = timePtr(due)
	c.HoldUntil = timePtr(hold)
	c.ClosedAt = timePtr(closed)
	return c, true, nil
}

// scanner は *sql.Rows と *sql.Row のどちらでも受けるための最小の口。
type scanner interface{ Scan(dest ...any) error }

func scanList(sc scanner) (Case, error) {
	var (
		c        Case
		tenant   sql.NullString
		reporter sql.NullString
		finger   sql.NullString
		due      sql.NullTime
		hold     sql.NullTime
		closed   sql.NullTime
		legacy   sql.NullString
	)
	if err := sc.Scan(
		&c.ID, &c.Number, &c.Source, &tenant, &c.Origin, &c.Kind, &c.Status, &c.ApprovalState,
		&c.Title, &reporter, &c.ScreenID, &c.FeatureID, &c.Environment, &c.Version, &c.URL,
		&finger, &due, &hold, &closed, &legacy,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return Case{}, fmt.Errorf("store: 案件を読めない")
	}
	c.TenantRef = tenant.String
	c.ReporterRef = reporter.String
	c.Fingerprint = finger.String
	c.LegacyRef = legacy.String
	c.PromisedDue = timePtr(due)
	c.HoldUntil = timePtr(hold)
	c.ClosedAt = timePtr(closed)
	return c, nil
}

func timePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}
