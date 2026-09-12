// 添付の参照とメタ。**実体は持たない**（設計文書 §1）。
//
// 実体が消えても行は消さない。残すのは、元のファイル名・種類・サイズ・内容のハッシュ・
// 参照・いつ消えたか・なぜ消えたか。**ハッシュは消さない。**同じファイルが後から
// もう一度来たときの判定が、これだけで成立する（§6）。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// 消えた理由。設計が決めた値で、組織ごとに変わらない（スキーマ側も ENUM）。
//
// **理由を持つ。**日時を 1 本だけ持つと、「期限で消した」「人が消した」
// 「置き場に元から無かった」が全部同じ形になり、後から答えられなくなる（設計文書 §6）。
const (
	DeletedExpired = "expired"
	DeletedByUser  = "removed_by_user"
	DeletedMissing = "missing_at_store"
)

// NewAttachment は足す添付 1 件。
//
// **SHA256 は台帳が受け取ったバイト列から計算したもの**（設計文書 §3）。
// 送り主が計算した値を受け取る形にすると、保管したものが送られたものと同じである
// ことを台帳自身が言えなくなる。
type NewAttachment struct {
	Ref       string
	Filename  string
	Mime      string
	SizeBytes uint64
	SHA256    string
}

// Attachment は読み出した添付 1 件。
type Attachment struct {
	ID        uint64
	Ref       string
	Filename  string
	Mime      string
	SizeBytes uint64
	SHA256    string
	Deleted   bool
	Reason    string
}

const insertAttachmentSQL = `
INSERT INTO case_attachments (case_id, ref, filename, mime, size_bytes, sha256)
VALUES (?,?,?,?,?,?)`

// AddAttachment は添付を 1 件足す。
func (s *Store) AddAttachment(ctx context.Context, f Filter, number string, a NewAttachment) (id uint64, found bool, err error) {
	caseID, found, err := caseIDFor(ctx, s.db, f, number)
	if err != nil || !found {
		return 0, found, err
	}
	res, err := s.db.ExecContext(ctx, insertAttachmentSQL,
		caseID, a.Ref, a.Filename, a.Mime, a.SizeBytes, a.SHA256)
	if err != nil {
		return 0, true, fmt.Errorf("store: 添付を足せない")
	}
	n, err := res.LastInsertId()
	if err != nil {
		return 0, true, fmt.Errorf("store: 足した添付の id を読めない")
	}
	return uint64(n), true, nil
}

const usedBytesSQL = `
SELECT COALESCE(SUM(size_bytes), 0) FROM case_attachments WHERE deleted_at IS NULL`

// UsedBytes は、まだ実体が在る添付の合計サイズを返す。
//
// **上限に当たったら受け付けない**（設計文書 §6）。古いものから押し出す形にすると、
// まだ必要なものが消える。
func (s *Store) UsedBytes(ctx context.Context) (uint64, error) {
	var n uint64
	if err := s.db.QueryRowContext(ctx, usedBytesSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 添付の合計サイズを引けない")
	}
	return n, nil
}

const findAttachmentSQL = `
SELECT a.id, a.ref, a.filename, a.mime, a.size_bytes, a.sha256, a.deleted_at, a.deleted_reason
  FROM case_attachments a
  JOIN cases c ON c.id = a.case_id
 WHERE a.id = ? AND c.number = ?`

// FindAttachment は案件の添付を 1 件引く。
//
// **番号と添付の id の両方で照合する。**id だけで引くと、自分の範囲の案件の番号を
// 添えるだけで、他の案件の添付を消せてしまう。
func (s *Store) FindAttachment(ctx context.Context, f Filter, number string, id uint64) (Attachment, bool, error) {
	scope, args := f.where()
	args = append([]any{id, number}, args...)

	var (
		a       Attachment
		deleted sql.NullTime
		reason  sql.NullString
	)
	err := s.db.QueryRowContext(ctx, findAttachmentSQL+scope, args...).
		Scan(&a.ID, &a.Ref, &a.Filename, &a.Mime, &a.SizeBytes, &a.SHA256, &deleted, &reason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Attachment{}, false, nil
	case err != nil:
		return Attachment{}, false, fmt.Errorf("store: 添付を引けない")
	}
	a.Deleted = deleted.Valid
	a.Reason = reason.String
	return a, true, nil
}

const markAttachmentDeletedSQL = `
UPDATE case_attachments SET deleted_at = UTC_TIMESTAMP(), deleted_reason = ?
 WHERE id = ? AND deleted_at IS NULL`

// MarkAttachmentDeleted は「実体はもう無い」の印を付ける。**行は消さない。**
//
// 既に印が付いている行は触らない。理由と日時が最初に消えたときのものであり続ける。
func (s *Store) MarkAttachmentDeleted(ctx context.Context, id uint64, reason string) error {
	if _, err := s.db.ExecContext(ctx, markAttachmentDeletedSQL, reason, id); err != nil {
		return fmt.Errorf("store: 添付に削除の印を付けられない")
	}
	return nil
}
