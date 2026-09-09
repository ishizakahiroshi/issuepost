# Agent Entry Point (issuepost)

このリポジトリの運用ガイダンスは `CLAUDE.md` を正本とする。

- プロジェクト概要・ルール: `./CLAUDE.md`
- ユーザー向けドキュメント: `./README.md`
- ローカル/プライベート追記（存在する場合・コミットしない）: `./CLAUDE.local.md` / `./AGENTS.local.md` / `./docs/local/`

個人/グローバル AI ルールは意図的にこのリポジトリの外に置く。各 AI ツールの
グローバル設定を使うこと。本ファイルは fresh public clone でも有効に保つ。

## Non-negotiables (full detail in CLAUDE.md)

- **これは public リポジトリ。** 実運用の接続先・組織名・システム名・人名・識別番号を、追跡対象ファイルへ書かない
- **設計が固まるまで実装を始めない。** 表の形と受け口の契約が先
- **中継を作らない。** 案件は持ち主のアプリが自分で送る。中央は受け取ったものを加工せず保管する
- ビルド・コミット禁止、secrets-scan 責務、plan/bugfix/pending md の作成ルール等の AI 作業共通ルールは、各利用者のグローバル AI 設定に従う（作者環境の例: `~/.claude/CLAUDE.md` および `~/.claude/guides/`）
- secrets-scan のこのリポジトリの配線（scanner パス・手動実行コマンド等）は `CLAUDE.md` の「secrets-scan（このリポジトリの配線）」節を参照
- `docs/obsidian/README.md` があれば索引として読み、知識記録は repo 相対の `docs/obsidian` を使う。欠損時に `docs/local` へ黙って fallback しない

ガイダンス間で矛盾が出たら `CLAUDE.md` を優先する。

<!-- many-ai-cli の承認マーカーブロックはここに自動注入される。本ファイルでは持たない。 -->
