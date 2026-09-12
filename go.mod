module github.com/ishizakahiroshi/issuepost

go 1.22

// 受け口の土台（設定・認証・エラーの語彙・1 件 1 行のログ）。
// 受け口を持つサービスならどれも同じになる部分を、写さずに 1 本から使う。
require github.com/ishizakahiroshi/doorpost v0.1.0

// 台帳の中身を置く MariaDB のドライバ。
// 版が v1.10.0 ではなく v1.8.1 なのは、v1.10.0 が go 1.24 以上を要求し、
// このリポジトリの go 1.22 では読み込めないため。ドライバ自体は同じもの。
require github.com/go-sql-driver/mysql v1.8.1

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	// 設定を TOML で読むための依存。いまは doorpost 越しに使う。
	// 設定にコメントが書けないと、鍵と許可 IP の横に「それが何の鍵か」を残せない。
	github.com/BurntSushi/toml v1.4.0 // indirect
)
