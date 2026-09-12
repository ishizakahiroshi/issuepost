package api

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishizakahiroshi/doorpost/config"
)

// テストの fixture はすべて合成データ。
// IP は RFC 5737 のドキュメント用（192.0.2.0/24 / 203.0.113.0/24）を使う。

const exampleTOML = `
listen = "127.0.0.1:8090"

[database]
host     = "127.0.0.1"
port     = 3306
name     = "issuepost"
user     = "issuepost"
password = "synthetic-password-for-test"
max_open_conns = 10

[[apps]]
name      = "app_one"
keys      = ["key-for-test----------------------------"]
envs      = ["production", "staging"]
allow_ips = ["192.0.2.1"]

[[apps]]
name      = "operator_console"
keys      = ["another-key-for-test--------------------"]
envs      = ["production"]
allow_ips = ["192.0.2.1"]

[[scopes]]
app    = "app_one"
source = ["app_one"]
tenant = ["tenant_a"]

[[scopes]]
app    = "operator_console"
source = ["*"]
tenant = ["*"]

[kinds]
values = ["bug", "request", "question", "note"]
requires_approval = ["request"]

[statuses]
values   = ["new", "investigated", "in_progress", "done", "wont_do"]
open     = ["new", "investigated", "in_progress"]
terminal = ["done", "wont_do"]
initial  = "new"
waiting  = { new = "us", investigated = "them", in_progress = "us", done = "none", wont_do = "none" }

[approval_states]
values       = ["not_required", "pending", "approved", "rejected", "on_hold"]
initial      = "pending"
initial_free = "not_required"
hold         = "on_hold"
`

func decode(t *testing.T, s string) (*Config, error) {
	t.Helper()
	var cfg Config
	err := config.Decode(strings.NewReader(s), &cfg)
	return &cfg, err
}

func TestDecodeExample(t *testing.T) {
	cfg, err := decode(t, exampleTOML)
	if err != nil {
		t.Fatalf("雛形と同じ形が読めない: %v", err)
	}
	if cfg.Statuses.Initial != "new" {
		t.Errorf("initial が読めていない: %q", cfg.Statuses.Initial)
	}
	if !cfg.Kinds.NeedsApproval("request") || cfg.Kinds.NeedsApproval("bug") {
		t.Error("requires_approval が読めていない")
	}
	if cfg.Approvals.For(true) != "pending" || cfg.Approvals.For(false) != "not_required" {
		t.Error("承認の初期値が読めていない")
	}
	if s := cfg.ScopeFor("app_one"); s == nil {
		t.Fatal("app_one の範囲が引けない")
	}
	if cfg.ScopeFor("unknown_app") != nil {
		t.Error("知らないアプリの範囲が引けてしまう")
	}
}

// リポジトリに置いてある雛形が、そのまま起動できる形であること。
// 雛形が通らない状態で配ると、最初に触る人が設定の書き方から疑うことになる。
func TestExampleFileLoads(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.toml")
	var cfg Config
	if err := config.Load(path, &cfg); err != nil {
		t.Fatalf("%s が読めない: %v", path, err)
	}
	// 雛形は、案件を入れられる合言葉と、横断で見るだけの合言葉の両方を示す。
	if s := cfg.ScopeFor("app_one"); s == nil {
		t.Fatal("雛形に案件を入れられる合言葉が無い")
	} else if _, ok := s.IntakeSource(); !ok {
		t.Error("雛形の app_one で案件を入れられない")
	}
	if s := cfg.ScopeFor("operator_console"); s == nil {
		t.Fatal("雛形に横断で見る合言葉が無い")
	} else if _, ok := s.IntakeSource(); ok {
		t.Error("横断の合言葉で案件を入れられてしまう")
	}
}

// 知らないキーで起動しない。綴りを間違えた設定が黙って既定値で動き出さないため。
func TestDecodeRejectsUnknownKey(t *testing.T) {
	_, err := decode(t, exampleTOML+"\nunknown_key = 1\n")
	if err == nil {
		t.Fatal("知らないキーが通ってしまった")
	}
}

// 設定の壊れ方を 1 つずつ見る。どれも起動しないことが正しい。
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{
			"範囲の無いアプリがある",
			strings.Replace(exampleTOML, `[[scopes]]
app    = "operator_console"
source = ["*"]
tenant = ["*"]
`, "", 1),
		},
		{
			"知らないアプリの範囲がある",
			exampleTOML + "\n[[scopes]]\napp = \"ghost\"\nsource = [\"ghost\"]\ntenant = [\"*\"]\n",
		},
		{
			"同じアプリの範囲が 2 つある",
			exampleTOML + "\n[[scopes]]\napp = \"app_one\"\nsource = [\"app_one\"]\ntenant = [\"*\"]\n",
		},
		{
			"範囲が空",
			strings.Replace(exampleTOML, `source = ["app_one"]`, `source = []`, 1),
		},
		{
			"全部と具体値を混ぜている",
			strings.Replace(exampleTOML, `source = ["app_one"]`, `source = ["app_one", "*"]`, 1),
		},
		{
			"範囲に空の要素がある",
			strings.Replace(exampleTOML, `tenant = ["tenant_a"]`, `tenant = ["tenant_a", ""]`, 1),
		},
		{
			"initial が open に無い",
			strings.Replace(exampleTOML, `initial  = "new"`, `initial  = "done"`, 1),
		},
		{
			"open と terminal が重なっている",
			strings.Replace(exampleTOML, `terminal = ["done", "wont_do"]`, `terminal = ["done", "wont_do", "new"]`, 1),
		},
		{
			"terminal に一覧に無い値がある",
			strings.Replace(exampleTOML, `terminal = ["done", "wont_do"]`, `terminal = ["done", "archived"]`, 1),
		},
		{
			"requires_approval に一覧に無い種別がある",
			strings.Replace(exampleTOML, `requires_approval = ["request"]`, `requires_approval = ["improvement"]`, 1),
		},
		{
			"承認の初期値が一覧に無い",
			strings.Replace(exampleTOML, `initial      = "pending"`, `initial      = "waiting"`, 1),
		},
		{
			"waiting に抜けがある",
			strings.Replace(exampleTOML,
				`waiting  = { new = "us", investigated = "them", in_progress = "us", done = "none", wont_do = "none" }`,
				`waiting  = { new = "us" }`, 1),
		},
		{
			"種別の一覧が空",
			strings.Replace(exampleTOML, `values = ["bug", "request", "question", "note"]`, `values = []`, 1),
		},
		{
			"データベースの名前が空",
			strings.Replace(exampleTOML, `name     = "issuepost"`, `name     = ""`, 1),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := decode(t, c.toml); err == nil {
				t.Fatal("起動してはいけない設定が通った")
			}
		})
	}
}

func TestScopeMatching(t *testing.T) {
	fixed := &Scope{App: "a", Source: []string{"app_one"}, Tenant: []string{"tenant_a", "tenant_b"}}
	wild := &Scope{App: "b", Source: []string{"*"}, Tenant: []string{"*"}}

	if !fixed.AllowsSource("app_one") || fixed.AllowsSource("app_two") {
		t.Error("source の範囲が効いていない")
	}
	if !fixed.AllowsTenant("tenant_b") || fixed.AllowsTenant("tenant_c") {
		t.Error("tenant の範囲が効いていない")
	}
	if !wild.AllowsSource("anything") || !wild.AllowsTenant("anything") {
		t.Error("全部の範囲が効いていない")
	}

	if s, ok := fixed.IntakeSource(); !ok || s != "app_one" {
		t.Errorf("案件を入れる source が決まらない: %q %v", s, ok)
	}
	if _, ok := wild.IntakeSource(); ok {
		t.Error("横断の合言葉で案件を入れられてしまう")
	}

	// 顧客が 2 つある範囲は、本文が書かないと決まらない。
	if _, ok := fixed.DefaultTenant(); ok {
		t.Error("顧客が複数あるのに既定が決まってしまう")
	}
	if v, ok := wild.DefaultTenant(); !ok || v != "" {
		t.Errorf("顧客で分かれていないアプリは空になるはず: %q %v", v, ok)
	}
	single := &Scope{App: "c", Source: []string{"app_one"}, Tenant: []string{"tenant_a"}}
	if v, ok := single.DefaultTenant(); !ok || v != "tenant_a" {
		t.Errorf("顧客が 1 つならそれになるはず: %q %v", v, ok)
	}
}
