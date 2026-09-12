// Package store は台帳そのもの（MariaDB）への読み書きを持つ。
//
// ここが受け持つのは 2 つだけ。案件を 1 件保存することと、そのときに番号を発行すること。
// 番号を発行するのは台帳だけで、アプリは発行しない（設計文書 §3）。
//
// 受け口の都合（どの語で断るか・誰に何を見せるか）はここへ持ち込まない。
// この層が知っているのは「この行を入れてよいか」ではなく「この行をどう入れるか」だけで、
// 入れてよいかを決めるのは上の層である。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// duplicateEntry は MariaDB が一意制約違反に返す番号。
const duplicateEntry = 1062

const (
	defaultPort         = 3306
	defaultMaxOpenConns = 10
)

// Database は台帳のデータベースへの繋ぎ方。
//
// DSN を 1 本の文字列で受けずに項目で受けるのは、時刻の扱いを設定する人に委ねないため。
// 日時はすべて UTC で保存すると決めてあり（設計文書の冒頭）、接続のタイムゾーンが 1 つ
// ずれると「期限を過ぎた」の境界が見る場所で 1 日ずれる。DSN を自分で組み立てれば、
// そこは設定の書き方によらず固定できる。
type Database struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Name     string `toml:"name"`
	User     string `toml:"user"`
	Password string `toml:"password"`

	// MaxOpenConns はこの台帳が張る接続の上限。隣のサービスと同じマシンに載るので、
	// 接続数も自分で決める（設計文書 §7）。0 なら既定値。
	MaxOpenConns int `toml:"max_open_conns"`
}

// Validate は繋ぎ方の設定を検証する。
func (d Database) Validate() error {
	if strings.TrimSpace(d.Host) == "" {
		return fmt.Errorf("database.host が空")
	}
	if d.Port < 0 || d.Port > 65535 {
		return fmt.Errorf("database.port が範囲外")
	}
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("database.name が空")
	}
	if strings.TrimSpace(d.User) == "" {
		return fmt.Errorf("database.user が空")
	}
	if d.MaxOpenConns < 0 {
		return fmt.Errorf("database.max_open_conns が負")
	}
	// password を空で弾かない。接続方式によっては空が正しいことがあり、
	// ここで弾くと「設定できるのに起動しない」状態を作る。
	return nil
}

// DSN は接続文字列を組み立てる。
//
// **戻り値にはパスワードが入る。ログにも応答にも出さない。**
func (d Database) DSN() string {
	c := mysql.NewConfig()
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(d.Host, strconv.Itoa(d.port()))
	c.User = d.User
	c.Passwd = d.Password
	c.DBName = d.Name
	// 照合順序はスキーマ（sql/0001_initial.sql）と揃える。
	c.Collation = "utf8mb4_unicode_ci"
	// DATETIME を time.Time として、しかも UTC として読む。
	c.ParseTime = true
	c.Loc = time.UTC
	// 接続側のタイムゾーンも UTC に固定する。ParseTime だけでは、サーバが
	// CURRENT_TIMESTAMP で入れる値のほうがサーバの現地時刻のままになる。
	c.Params = map[string]string{"time_zone": "'+00:00'"}
	// 値をプレースホルダのまま送る。文字列に組み立て直させない。
	c.InterpolateParams = false
	return c.FormatDSN()
}

func (d Database) port() int {
	if d.Port == 0 {
		return defaultPort
	}
	return d.Port
}

func (d Database) maxOpenConns() int {
	if d.MaxOpenConns == 0 {
		return defaultMaxOpenConns
	}
	return d.MaxOpenConns
}

// NewCase は保存する案件 1 件。
//
// 受け口が検証を通したものだけが来る。空文字は「送られてこなかった」を表し、
// NULL を入れる列はここで振り分ける。
type NewCase struct {
	Source        string
	TenantRef     string
	Origin        string
	Kind          string
	Status        string
	ApprovalState string
	Title         string
	Body          string
	ReporterRef   string // origin=detected のときは空
	ScreenID      string
	FeatureID     string
	Environment   string
	Version       string
	URL           string
	Fingerprint   string // origin=human のときは空
	PromisedDue   string // 空、または YYYY-MM-DD
	LegacyRef     string // 空なら二重登録の守りを持たない
}

// Created は保存の結果。
type Created struct {
	Number string
	Status string
	// Existed は、同じ (source, legacy_ref) が既にあってそれを返したこと。
	// 同じものを 2 回送っても 2 件にならない（設計文書 §3）。
	Existed bool
}

// Store は台帳への接続。
type Store struct {
	db *sql.DB
}

// Open は接続を開く。実際に繋がるかは [Store.Ping] で確かめる。
func Open(d Database) (*Store, error) {
	db, err := sql.Open("mysql", d.DSN())
	if err != nil {
		// エラー文に DSN を載せない（パスワードが入っている）。
		return nil, fmt.Errorf("store: データベースを開けない")
	}
	db.SetMaxOpenConns(d.maxOpenConns())
	db.SetMaxIdleConns(d.maxOpenConns())
	db.SetConnMaxLifetime(time.Hour)
	return &Store{db: db}, nil
}

// Ping は実際に繋がるかを確かめる。
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: データベースへ繋がらない")
	}
	return nil
}

// Close は接続を閉じる。
func (s *Store) Close() error { return s.db.Close() }

const insertCaseSQL = `
INSERT INTO cases
  (source, seq, number, tenant_ref, origin, kind, status, approval_state,
   title, body, reporter_ref, screen_id, feature_id, environment, version, url,
   fingerprint, promised_due, legacy_ref)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

const insertEventSQL = `
INSERT INTO case_events (case_id, event_type, from_value, to_value, note, actor_ref)
VALUES (?,?,NULL,?,'',?)`

const findByLegacySQL = `
SELECT number, status FROM cases WHERE source = ? AND legacy_ref = ?`

// CreateCase は案件を 1 件保存し、発行した番号を返す。
//
// 番号の発行と案件の保存は同じトランザクションの中で行う。分けると、番号だけ進んで
// 案件が入らない状態が残る。
//
// 保存できなければエラーを返す。ここで預かることも、あとで送り直すこともしない
// （設計文書 §3「送れなかったとき」）。番号は発行されないので、呼び出し側が
// 「受け付けました」と番号を出せる経路が存在しない。
func (s *Store) CreateCase(ctx context.Context, c NewCase) (Created, error) {
	// 送り主が自分側の ID を付けているなら、先に見る。
	// 採番してから一意制約で弾かれるより、番号を飛ばさずに済む。
	if c.LegacyRef != "" {
		got, ok, err := s.findByLegacy(ctx, c.Source, c.LegacyRef)
		if err != nil {
			return Created{}, err
		}
		if ok {
			return got, nil
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Created{}, fmt.Errorf("store: トランザクションを開始できない")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	seq, err := allocSeq(ctx, tx, c.Source)
	if err != nil {
		return Created{}, err
	}
	number := FormatNumber(c.Source, seq)

	res, err := tx.ExecContext(ctx, insertCaseSQL,
		c.Source, seq, number, c.TenantRef, c.Origin, c.Kind, c.Status, c.ApprovalState,
		c.Title, c.Body, nullIfEmpty(c.ReporterRef), c.ScreenID, c.FeatureID,
		c.Environment, c.Version, c.URL,
		nullIfEmpty(c.Fingerprint), nullIfEmpty(c.PromisedDue), nullIfEmpty(c.LegacyRef),
	)
	if err != nil {
		// 先に見た時点では無かったが、その隙に同じものが入った。
		// 番号は 1 つ飛ぶが、番号が連番である必要は無い（指すのは案件であって順番ではない）。
		if isDuplicate(err) && c.LegacyRef != "" {
			_ = tx.Rollback()
			got, ok, ferr := s.findByLegacy(ctx, c.Source, c.LegacyRef)
			if ferr != nil {
				return Created{}, ferr
			}
			if ok {
				return got, nil
			}
		}
		return Created{}, fmt.Errorf("store: 案件を保存できない")
	}

	id, err := res.LastInsertId()
	if err != nil {
		return Created{}, fmt.Errorf("store: 保存した案件の id を読めない")
	}

	// 状態と承認の最初の 1 行を残す。from_value は NULL（前の値が無い）。
	// いまの値は上書きされるので、上書きの前に 1 行入れておかないと経緯が始まらない。
	actor := c.ReporterRef // 検知には報告者がいないので空になる
	if _, err := tx.ExecContext(ctx, insertEventSQL, id, "status", c.Status, actor); err != nil {
		return Created{}, fmt.Errorf("store: 状態の経緯を残せない")
	}
	if _, err := tx.ExecContext(ctx, insertEventSQL, id, "approval", c.ApprovalState, actor); err != nil {
		return Created{}, fmt.Errorf("store: 承認の経緯を残せない")
	}

	if err := tx.Commit(); err != nil {
		return Created{}, fmt.Errorf("store: 保存を確定できない")
	}
	committed = true

	return Created{Number: number, Status: c.Status}, nil
}

func (s *Store) findByLegacy(ctx context.Context, source, legacy string) (Created, bool, error) {
	var number, status string
	err := s.db.QueryRowContext(ctx, findByLegacySQL, source, legacy).Scan(&number, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Created{}, false, nil
	case err != nil:
		return Created{}, false, fmt.Errorf("store: 既存の案件を引けない")
	}
	return Created{Number: number, Status: status, Existed: true}, true, nil
}

const allocSeqSQL = `
INSERT INTO case_number_sequences (source, next_seq) VALUES (?, 2)
  ON DUPLICATE KEY UPDATE next_seq = LAST_INSERT_ID(next_seq) + 1`

// allocSeq は source の中での連番を 1 つ払い出す。
//
// 新しい source のときは行を作って 1 を返し、既にあるときは現在値を返して次回ぶんを足す。
// UPDATE だけにすると、行が無いときに 0 行ヒットでエラーも出ず、番号が黙って壊れる。
//
// **新しい source のときに LAST_INSERT_ID() を読んではいけない。**
// この表には AUTO_INCREMENT の列が無いので、素の INSERT は LAST_INSERT_ID() を
// 更新しない。読むと接続に残っている前の値（新しい接続なら 0）が返る。
// 行が作られたかどうかは、影響行数で分かる（INSERT なら 1・ON DUPLICATE の UPDATE なら 2）。
func allocSeq(ctx context.Context, tx *sql.Tx, source string) (uint64, error) {
	res, err := tx.ExecContext(ctx, allocSeqSQL, source)
	if err != nil {
		return 0, fmt.Errorf("store: 番号を払い出せない")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: 番号の払い出しを確認できない")
	}
	if n == 1 {
		// 行が無かった。next_seq に 2 を入れたので、いま払い出すのは 1。
		return 1, nil
	}
	var seq uint64
	if err := tx.QueryRowContext(ctx, `SELECT LAST_INSERT_ID()`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: 払い出した番号を読めない")
	}
	if seq == 0 {
		return 0, fmt.Errorf("store: 払い出した番号が 0")
	}
	return seq, nil
}

// FormatNumber は公開される案件番号を組み立てる。
// スキーマ側も number = CONCAT(source, '-', seq) を CHECK で縛っているので、
// 形を変えるとデータベースが受け取らない。
func FormatNumber(source string, seq uint64) string {
	return source + "-" + strconv.FormatUint(seq, 10)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == duplicateEntry
}
