// 受け取ったあとの書き込み。状態の変更と、案件にぶら下がるもの。
//
// **どれも番号から入る**（[caseIDFor]）。案件を引くところで範囲が切られるので、
// 範囲の外の番号へは 1 行も書けない。書く側が毎回条件を足す形にすると、
// 8 本のうち 1 本で足し忘れたときに、別の顧客の案件へ書けてしまう。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Changes は案件に加える変更。**nil の項目は触らない。**
//
// 日付の 2 本は「空文字で空にする」形にしてある。「触らない」と「空にする」を
// 区別できないと、保留を解除する操作が書けない。
type Changes struct {
	Status        *string
	ApprovalState *string
	PromisedDue   *string // "" なら空にする
	HoldUntil     *string // "" なら空にする

	// Terminal は、変更後の状態が終端かどうか。Status が nil なら nil。
	//
	// **終端かどうかを決めるのは設定**（statuses.terminal）なので、この層は判定しない。
	// 受け口が引いた答えを受け取り、closed_at の出し入れだけを行う。
	Terminal *bool

	// ActorRef は変えた人。自動で変わるもの（期限切れの繰り上げなど）では空。
	ActorRef string
	// Note はこの変更の理由。却下の理由などが入る。
	// この変更で残る経緯の行すべてに同じものを付ける。
	Note string
}

// Empty は変えるものが 1 つも無いかを返す。
func (c Changes) Empty() bool {
	return c.Status == nil && c.ApprovalState == nil && c.PromisedDue == nil && c.HoldUntil == nil
}

const lockCaseSQL = `
SELECT c.id, c.status, c.approval_state FROM cases c WHERE c.number = ?`

const insertChangeEventSQL = `
INSERT INTO case_events (case_id, event_type, from_value, to_value, note, actor_ref)
VALUES (?,?,?,?,?,?)`

// UpdateCase は状態・承認・期限 2 本を変え、変えたぶんの経緯を残す。
//
// **いまの値と同じものを送られたときは、経緯を残さない。**残すと、画面の保存ボタンを
// 2 回押しただけで「変えた」行が増え、経緯が経緯でなくなる。
//
// 戻り値は変更後の案件。範囲の外・番号が無いときは found=false。
func (s *Store) UpdateCase(ctx context.Context, f Filter, number string, ch Changes) (Case, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Case{}, false, fmt.Errorf("store: トランザクションを開始できない")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	scope, args := f.where()
	args = append([]any{number}, args...)

	var (
		id       uint64
		status   string
		approval string
	)
	// FOR UPDATE で行を押さえる。押さえないと、読んだ値を from_value に書いている
	// 間に別の変更が入り、経緯が実際とつながらない鎖になる。
	err = tx.QueryRowContext(ctx, lockCaseSQL+scope+" FOR UPDATE", args...).
		Scan(&id, &status, &approval)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Case{}, false, nil
	case err != nil:
		return Case{}, false, fmt.Errorf("store: 案件を引けない")
	}

	var (
		sets     []string
		setArgs  []any
		events   [][]any // case_id, event_type, from, to, note, actor
		statusTo = status
	)

	if ch.Status != nil && *ch.Status != status {
		sets = append(sets, "status = ?")
		setArgs = append(setArgs, *ch.Status)
		events = append(events, []any{id, "status", status, *ch.Status, ch.Note, ch.ActorRef})
		statusTo = *ch.Status
	}
	if ch.ApprovalState != nil && *ch.ApprovalState != approval {
		sets = append(sets, "approval_state = ?")
		setArgs = append(setArgs, *ch.ApprovalState)
		events = append(events, []any{id, "approval", approval, *ch.ApprovalState, ch.Note, ch.ActorRef})
	}
	if ch.PromisedDue != nil {
		sets = append(sets, "promised_due = ?")
		setArgs = append(setArgs, nullIfEmpty(*ch.PromisedDue))
	}
	if ch.HoldUntil != nil {
		sets = append(sets, "hold_until = ?")
		setArgs = append(setArgs, nullIfEmpty(*ch.HoldUntil))
	}
	// 終端に入った瞬間に closed_at を記録し、外れたら空へ戻す（設計文書 §2）。
	// 終端から別の終端へ動いたときは、最初に終わった日時のままにする。
	// COALESCE にしてあるのはそのため。添付の寿命の起点が後ろへずれない。
	if ch.Terminal != nil && statusTo != status {
		if *ch.Terminal {
			sets = append(sets, "closed_at = COALESCE(closed_at, UTC_TIMESTAMP())")
		} else {
			sets = append(sets, "closed_at = NULL")
		}
	}

	if len(sets) > 0 {
		q := "UPDATE cases SET " + strings.Join(sets, ", ") + " WHERE id = ?"
		if _, err := tx.ExecContext(ctx, q, append(setArgs, id)...); err != nil {
			return Case{}, false, fmt.Errorf("store: 案件を更新できない")
		}
	}
	for _, e := range events {
		if _, err := tx.ExecContext(ctx, insertChangeEventSQL, e...); err != nil {
			return Case{}, false, fmt.Errorf("store: 経緯を残せない")
		}
	}

	got, found, err := getCase(ctx, tx, f, number)
	if err != nil {
		return Case{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Case{}, false, fmt.Errorf("store: 変更を確定できない")
	}
	committed = true
	return got, found, nil
}

const insertPersonSQL = `
INSERT INTO case_people (case_id, reporter_ref) VALUES (?,?)`

// AddPerson は「同じことに当たった人」を 1 人足す。
//
// **同じ人を 2 回送っても 2 人に数えない。**人数が優先順位の根拠になる以上、
// ここは一意制約で受ける。既にいた場合は added=false を返す（エラーにしない）。
// 送った側から見れば「その人は数えられている」ことに変わりはない。
func (s *Store) AddPerson(ctx context.Context, f Filter, number, reporterRef string) (added, found bool, err error) {
	id, found, err := caseIDFor(ctx, s.db, f, number)
	if err != nil || !found {
		return false, found, err
	}
	if _, err := s.db.ExecContext(ctx, insertPersonSQL, id, reporterRef); err != nil {
		if isDuplicate(err) {
			return false, true, nil
		}
		return false, true, fmt.Errorf("store: 人を足せない")
	}
	return true, true, nil
}

const insertReplySQL = `
INSERT INTO case_replies (case_id, body, author_ref) VALUES (?,?,?)`

// AddReply は返事を 1 件足す。
//
// **delivered_at は入れない。**台帳に保存できたことと、相手に届いたことは別で、
// 届けたと言えるのは通知の経路が受け取ったときだけ（設計文書 §3 と同じ扱い）。
func (s *Store) AddReply(ctx context.Context, f Filter, number, body, authorRef string) (id uint64, found bool, err error) {
	caseID, found, err := caseIDFor(ctx, s.db, f, number)
	if err != nil || !found {
		return 0, found, err
	}
	res, err := s.db.ExecContext(ctx, insertReplySQL, caseID, body, authorRef)
	if err != nil {
		return 0, true, fmt.Errorf("store: 返事を足せない")
	}
	n, err := res.LastInsertId()
	if err != nil {
		return 0, true, fmt.Errorf("store: 足した返事の id を読めない")
	}
	return uint64(n), true, nil
}

const insertLinkSQL = `
INSERT INTO case_links (case_id, link_type, ref) VALUES (?,?,?)`

const findLinkSQL = `
SELECT id FROM case_links WHERE case_id = ? AND link_type = ? AND ref = ?`

// AddLink はコミット・文書・URL を 1 つ足す。
//
// **同じものを 2 回送っても 1 行**（設計文書 §3）。一意制約で受け、2 回目は
// 既にある行の id を返す。ビルドの仕組みが再実行されるたびに同じコミットが
// 積み上がると、直した証拠が読めなくなる。
func (s *Store) AddLink(ctx context.Context, f Filter, number, linkType, ref string) (id uint64, added, found bool, err error) {
	caseID, found, err := caseIDFor(ctx, s.db, f, number)
	if err != nil || !found {
		return 0, false, found, err
	}
	res, err := s.db.ExecContext(ctx, insertLinkSQL, caseID, linkType, ref)
	if err != nil {
		if !isDuplicate(err) {
			return 0, false, true, fmt.Errorf("store: つながりを足せない")
		}
		var existing uint64
		if err := s.db.QueryRowContext(ctx, findLinkSQL, caseID, linkType, ref).Scan(&existing); err != nil {
			return 0, false, true, fmt.Errorf("store: 既にあるつながりを引けない")
		}
		return existing, false, true, nil
	}
	n, err := res.LastInsertId()
	if err != nil {
		return 0, false, true, fmt.Errorf("store: 足したつながりの id を読めない")
	}
	return uint64(n), true, true, nil
}
