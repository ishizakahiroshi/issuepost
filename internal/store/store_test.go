package store

import (
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// このパッケージのテストはデータベースへ繋がない。
// 検査できるのは、接続文字列の組み立てと番号の形だけ。
// SQL そのものは MariaDB のある環境で 1 回流すまで未検証のまま残る。

func testDatabase() Database {
	return Database{
		Host:     "127.0.0.1",
		Port:     3306,
		Name:     "issuepost",
		User:     "issuepost",
		Password: "synthetic-password-for-test",
	}
}

// DSN は接続のタイムゾーンを固定する。
// 日時がすべて UTC で保存されることが、ここに乗っている。
//
// 文字列の一致ではなくドライバに読み直させる。DSN は既定値と同じ項目を書き出さないので
// （loc は既定が UTC なので現れない）、文字列を探すと「書いてあるか」しか分からない。
// 見たいのは「繋いだときに何になるか」のほう。
func TestDSNFixesUTC(t *testing.T) {
	c := parseDSN(t, testDatabase())

	if c.Loc != time.UTC {
		t.Errorf("接続のタイムゾーンが UTC でない: %v", c.Loc)
	}
	if !c.ParseTime {
		t.Error("DATETIME が time.Time として読まれない")
	}
	// サーバ側の CURRENT_TIMESTAMP も UTC で入るようにする。
	// ParseTime だけだと、サーバが自分で入れる値はサーバの現地時刻のままになる。
	if got := c.Params["time_zone"]; got != "'+00:00'" {
		t.Errorf("サーバ側のタイムゾーンが固定されていない: %q", got)
	}
	if c.Collation != "utf8mb4_unicode_ci" {
		t.Errorf("照合順序がスキーマと揃っていない: %q", c.Collation)
	}
	if c.InterpolateParams {
		t.Error("値を文字列へ組み立て直させている")
	}
	if c.Addr != "127.0.0.1:3306" || c.DBName != "issuepost" {
		t.Errorf("接続先が違う: %s %s", c.Addr, c.DBName)
	}
}

// ポートを書かなかったときは既定の 3306 になる。
func TestDSNDefaultPort(t *testing.T) {
	d := testDatabase()
	d.Port = 0
	if got := parseDSN(t, d).Addr; got != "127.0.0.1:3306" {
		t.Errorf("既定のポートが入っていない: %s", got)
	}
}

// parseDSN は組み立てた DSN をドライバに読み直させる。
// 戻り値にパスワードが入るので、失敗しても DSN そのものは出さない。
func parseDSN(t *testing.T, d Database) *mysql.Config {
	t.Helper()
	c, err := mysql.ParseDSN(d.DSN())
	if err != nil {
		t.Fatalf("組み立てた DSN をドライバが読めない: %v", err)
	}
	return c
}

func TestDatabaseValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Database)
		wantErr bool
	}{
		{"そのまま", func(*Database) {}, false},
		{"パスワードが空でも通る", func(d *Database) { d.Password = "" }, false},
		{"host が空", func(d *Database) { d.Host = "" }, true},
		{"name が空", func(d *Database) { d.Name = "" }, true},
		{"user が空", func(d *Database) { d.User = "" }, true},
		{"port が範囲外", func(d *Database) { d.Port = 70000 }, true},
		{"max_open_conns が負", func(d *Database) { d.MaxOpenConns = -1 }, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := testDatabase()
			c.mutate(&d)
			err := d.Validate()
			if c.wantErr && err == nil {
				t.Fatal("エラーになるはずが通った")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("通るはずがエラー: %v", err)
			}
		})
	}
}

// 番号は <source>-<seq>。スキーマ側も CHECK で同じ形を縛っている。
func TestFormatNumber(t *testing.T) {
	if got := FormatNumber("app_one", 234); got != "app_one-234" {
		t.Errorf("番号の形が違う: %s", got)
	}
	if got := FormatNumber("app_two", 1); got != "app_two-1" {
		t.Errorf("番号の形が違う: %s", got)
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Error("空文字は NULL になるはず")
	}
	if nullIfEmpty("u-10482") != any("u-10482") {
		t.Error("空でない値はそのまま入るはず")
	}
}
