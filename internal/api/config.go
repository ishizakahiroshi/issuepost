package api

// 台帳の設定。共通部分（待ち受け・鍵・許可 IP）は doorpost が持ち、
// ここには issuepost だけの項目を書く。
//
// **業務固有の語（種別・状態・承認状態）はコードに 1 つも書かない**（設計文書 §2）。
// 値は設定で定義し、受け口はその一覧に対して検査する。一覧に無い値は最初から弾かれる。
//
// 設定ファイルは TOML。鍵と許可 IP と語の一覧が並ぶファイルに「それが何か」を
// 書き残せることが、外部依存 1 本より価値がある。

import (
	"fmt"
	"strings"

	"github.com/ishizakahiroshi/doorpost/config"

	"github.com/ishizakahiroshi/issuepost/internal/blobs"
	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// Wildcard は見える範囲の「全部」。
//
// 文字列 1 つで表し、他の値と混ぜて書けない。混ぜられると
// 「全部と、そのうちの 1 つ」という読み方の要らない曖昧さが生まれる。
const Wildcard = "*"

// Config は台帳 1 台ぶんの設定。
type Config struct {
	config.Base

	Database store.Database `toml:"database"`

	// Scopes は合言葉ごとに読める範囲。[[apps]] の name で結ぶ。
	//
	// 設計文書 §4 は [[tokens]] に scope を内包した形で書いてあるが、
	// 鍵・許可 IP・環境名を持つのは doorpost の [[apps]] なので、範囲だけを
	// 別の表として持ち、name で対応させる。1 つのアプリにつき 1 つ必要。
	Scopes []Scope `toml:"scopes"`

	Kinds     Kinds     `toml:"kinds"`
	Statuses  Statuses  `toml:"statuses"`
	Approvals Approvals `toml:"approval_states"`

	Attachments Attachments `toml:"attachments"`

	scopeByApp map[string]*Scope
}

// Scope は 1 つの合言葉が触れる範囲。
//
// source と tenant は必ず配列で書く（設計文書 §4）。片方を文字列、片方を配列に
// すると、読む実装が 2 通りになる。
type Scope struct {
	App    string   `toml:"app"`
	Source []string `toml:"source"`
	Tenant []string `toml:"tenant"`
}

// Attachments は添付の受け取り方。
//
// **書かなければ添付の口だけが動かない。**置き場が決まっていない間も、
// 案件そのものは受け付けられる（設計文書 §6「添付が入らないことを理由に
// 案件を落とすほうが、損失が大きい」）。
type Attachments struct {
	blobs.Config

	// MaxFileBytes は 1 件の上限。超えたものは切らずに拒否する。
	MaxFileBytes int64 `toml:"max_file_bytes"`
	// TotalLimitBytes は置き場ぜんたいの上限。
	// **当たったら受け付けない。**古いものから押し出さない（§6）。
	TotalLimitBytes int64 `toml:"total_limit_bytes"`
}

func (a Attachments) validate() error {
	if err := a.Config.Validate(); err != nil {
		return err
	}
	if !a.Configured() {
		// 置き場を書いていないなら、上限も要らない。
		return nil
	}
	if a.MaxFileBytes <= 0 {
		return fmt.Errorf("attachments.max_file_bytes が空。上限の無い受け口を作らない")
	}
	if a.TotalLimitBytes <= 0 {
		return fmt.Errorf("attachments.total_limit_bytes が空。上限の無い置き場を作らない")
	}
	if a.MaxFileBytes > a.TotalLimitBytes {
		return fmt.Errorf("attachments.max_file_bytes が total_limit_bytes より大きい")
	}
	return nil
}

// Kinds は案件の種別。
type Kinds struct {
	Values           []string `toml:"values"`
	RequiresApproval []string `toml:"requires_approval"`
}

// Statuses は案件の状態。全アプリ・全画面が 1 セットを使う。
type Statuses struct {
	Values   []string `toml:"values"`
	Open     []string `toml:"open"`
	Terminal []string `toml:"terminal"`
	Initial  string   `toml:"initial"`
	// Waiting は「誰待ちか」の対応。状態は進み具合で、誰待ちとは別の軸なので設定で持つ。
	// 受け口は使わないが、同じ設定ファイルを画面が読むので、ここで形だけ検証する。
	Waiting map[string]string `toml:"waiting"`
}

// Approvals は承認状態。
type Approvals struct {
	Values      []string `toml:"values"`
	Initial     string   `toml:"initial"`
	InitialFree string   `toml:"initial_free"`
	// Hold は「保留」を表す識別子。保留の解除期限（hold_until）を持てるのは
	// この状態のときだけ（設計文書 §2）。
	//
	// **どの語が保留かをコードに書かない。**値は組織ごとに差し替えられるので、
	// 受け口が名指しできるのは設定を通したときだけ。書き忘れたら起動しない。
	Hold string `toml:"hold"`
}

// Validate は issuepost が足した項目を検証する。
// 共通部分の検証がすべて通ったあとで config.Decode から呼ばれる。
func (c *Config) Validate() error {
	if err := c.Database.Validate(); err != nil {
		return err
	}
	if err := c.Kinds.validate(); err != nil {
		return err
	}
	if err := c.Statuses.validate(); err != nil {
		return err
	}
	if err := c.Approvals.validate(); err != nil {
		return err
	}
	if err := c.Attachments.validate(); err != nil {
		return err
	}
	return c.indexScopes()
}

// indexScopes は合言葉と範囲を突き合わせ、引けるようにする。
//
// **範囲の無いアプリを「全部見える」と読まない。** 書き忘れたら起動しない。
// 空を全許可と読む実装は、書き忘れがそのまま穴になる。
func (c *Config) indexScopes() error {
	byApp := make(map[string]*Scope, len(c.Scopes))
	for i := range c.Scopes {
		s := &c.Scopes[i]
		if strings.TrimSpace(s.App) == "" {
			return fmt.Errorf("scopes[%d].app が空", i)
		}
		if byApp[s.App] != nil {
			return fmt.Errorf("scopes[%d].app が重複している: %s", i, s.App)
		}
		if c.AppNamed(s.App) == nil {
			return fmt.Errorf("scopes[%d].app に対応する apps が無い: %s", i, s.App)
		}
		if err := checkRange(fmt.Sprintf("scopes[%d].source", i), s.Source); err != nil {
			return err
		}
		if err := checkRange(fmt.Sprintf("scopes[%d].tenant", i), s.Tenant); err != nil {
			return err
		}
		byApp[s.App] = s
	}
	for i := range c.Apps {
		if byApp[c.Apps[i].Name] == nil {
			return fmt.Errorf("apps[%d](%s) に対応する scopes が無い", i, c.Apps[i].Name)
		}
	}
	c.scopeByApp = byApp
	return nil
}

// AppNamed は名前でアプリを引く。無ければ nil。
func (c *Config) AppNamed(name string) *config.App {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i]
		}
	}
	return nil
}

// ScopeFor はアプリの範囲を返す。検証を通った設定では必ず見つかる。
func (c *Config) ScopeFor(name string) *Scope {
	return c.scopeByApp[name]
}

func checkRange(label string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s が空。空のリストは「全部」ではない", label)
	}
	seen := make(map[string]bool, len(values))
	wild := false
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s に空の要素がある", label)
		}
		if seen[v] {
			return fmt.Errorf("%s に重複がある: %s", label, v)
		}
		seen[v] = true
		if v == Wildcard {
			wild = true
		}
	}
	if wild && len(values) > 1 {
		return fmt.Errorf("%s は %q を他の値と混ぜて書けない", label, Wildcard)
	}
	return nil
}

// Allows は値が範囲の中かを返す。
func allows(values []string, v string) bool {
	for _, x := range values {
		if x == Wildcard || x == v {
			return true
		}
	}
	return false
}

// AllowsSource は出どころのアプリが範囲の中かを返す。
func (s *Scope) AllowsSource(v string) bool { return allows(s.Source, v) }

// AllowsTenant は顧客が範囲の中かを返す。
func (s *Scope) AllowsTenant(v string) bool { return allows(s.Tenant, v) }

// IntakeSource は、この合言葉で案件を入れるときの source を返す。
//
// 範囲が具体的な 1 つに固定されているときだけ決まる。**合言葉が source を固定する**
// ことが、知らない接頭辞が勝手に採番されない理由そのもの（設計文書 §4）なので、
// 複数または "*" の合言葉は案件を入れられない（横断で見る画面の合言葉がこれに当たる）。
func (s *Scope) IntakeSource() (string, bool) {
	if len(s.Source) != 1 || s.Source[0] == Wildcard {
		return "", false
	}
	return s.Source[0], true
}

// DefaultTenant は、本文が顧客を書かなかったときに入れる値を返す。
//
// 範囲が "*" なら空（顧客で分かれていないアプリ・設計文書 §4）。
// 具体的な 1 つならそれ。複数あるときは決まらないので、本文に書いてもらう。
func (s *Scope) DefaultTenant() (string, bool) {
	if len(s.Tenant) == 1 {
		if s.Tenant[0] == Wildcard {
			return "", true
		}
		return s.Tenant[0], true
	}
	return "", false
}

// Has は種別が一覧にあるかを返す。
func (k Kinds) Has(v string) bool { return contains(k.Values, v) }

// NeedsApproval は種別が承認を要するかを返す。
func (k Kinds) NeedsApproval(v string) bool { return contains(k.RequiresApproval, v) }

func (k Kinds) validate() error {
	if err := checkVocabulary("kinds.values", k.Values); err != nil {
		return err
	}
	return checkSubset("kinds.requires_approval", k.RequiresApproval, k.Values)
}

// Has は状態が一覧にあるかを返す。
func (s Statuses) Has(v string) bool { return contains(s.Values, v) }

// IsTerminal は状態が終端かを返す。
//
// 終端に入った瞬間に closed_at が入り、外れたら空へ戻る（設計文書 §2）。
// 添付の寿命の起点なので、判定はこの 1 か所だけが持つ。
func (s Statuses) IsTerminal(v string) bool { return contains(s.Terminal, v) }

func (s Statuses) validate() error {
	if err := checkVocabulary("statuses.values", s.Values); err != nil {
		return err
	}
	if err := checkSubset("statuses.open", s.Open, s.Values); err != nil {
		return err
	}
	if err := checkSubset("statuses.terminal", s.Terminal, s.Values); err != nil {
		return err
	}
	for _, v := range s.Open {
		if contains(s.Terminal, v) {
			return fmt.Errorf("statuses: %q が open と terminal の両方にある", v)
		}
	}
	if strings.TrimSpace(s.Initial) == "" {
		return fmt.Errorf("statuses.initial が空")
	}
	// 受け口が入れる最初の値なので、開いている状態でなければならない。
	// 終端の値を入れると、入れた瞬間に closed_at を入れる話になり、
	// 届いたばかりの案件が「終わったもの」として数えられる。
	if !contains(s.Open, s.Initial) {
		return fmt.Errorf("statuses.initial が open に無い: %s", s.Initial)
	}
	if len(s.Waiting) > 0 {
		for k := range s.Waiting {
			if !contains(s.Values, k) {
				return fmt.Errorf("statuses.waiting に values に無い状態がある: %s", k)
			}
		}
		for _, v := range s.Values {
			if strings.TrimSpace(s.Waiting[v]) == "" {
				return fmt.Errorf("statuses.waiting に %q の行が無い", v)
			}
		}
	}
	return nil
}

func (a Approvals) validate() error {
	if err := checkVocabulary("approval_states.values", a.Values); err != nil {
		return err
	}
	if !contains(a.Values, a.Initial) {
		return fmt.Errorf("approval_states.initial が values に無い: %s", a.Initial)
	}
	if !contains(a.Values, a.InitialFree) {
		return fmt.Errorf("approval_states.initial_free が values に無い: %s", a.InitialFree)
	}
	if !contains(a.Values, a.Hold) {
		return fmt.Errorf("approval_states.hold が values に無い: %s", a.Hold)
	}
	return nil
}

// Has は承認状態が一覧にあるかを返す。
func (a Approvals) Has(v string) bool { return contains(a.Values, v) }

// For は種別に対する最初の承認状態を返す。
func (a Approvals) For(needsApproval bool) string {
	if needsApproval {
		return a.Initial
	}
	return a.InitialFree
}

func checkVocabulary(label string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s が空", label)
	}
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s に空の要素がある", label)
		}
		if seen[v] {
			return fmt.Errorf("%s に重複がある: %s", label, v)
		}
		seen[v] = true
	}
	return nil
}

func checkSubset(label string, sub, all []string) error {
	for _, v := range sub {
		if !contains(all, v) {
			return fmt.Errorf("%s に一覧に無い値がある: %s", label, v)
		}
	}
	return nil
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
