<!-- このファイルはプロジェクト固有ルールのみを書く。個人/グローバル AI ルール
（言語・確認スタイル・出力フォーマット等）は各 AI ツールのグローバル設定へ。
fresh public clone でも有効な内容に保つこと。 -->

# issuepost 開発ガイド

> **このファイルは索引であって本文ではない。** 全 AI セッションで全文がロードされるので、
> ルールの本文はここへ書かず、破る人が必ず開く場所（コード・検査スクリプト・skill・guide・台帳）へ置き、
> ここには索引の 1 行だけを残す。新しいルールを足す前に既存の CLAUDE.md・skill・guide・台帳を検索し、
> 正本が既にあれば参照だけにする。詳細は下記「設計原則の索引」。

## プロジェクト概要

複数のアプリが受け取った「案件」を 1 か所へ集める台帳。各アプリが自分で検知した異常も、
利用者が書いた不具合・要望・質問・メモも、**同じ 1 つの表**に入れる。受け口は 1 本で、
アプリはそこへ送るだけ。集めた側は、誰待ちで止まっているか・どこに問題が集中しているかを
1 画面で見せる。

**設計を先に書き、実装はその後。** いま存在するのは README と設計文書だけで、実装は無い。

## やらないこと（スコープ外）

- **中継**（あるアプリが別のアプリの案件を代理で集めて回す形）。持ち主が自分で送る形だけを作る
- **受け取ったものの加工。** 中央は保管と集計だけをする。分類や書き換えをしない
- 課題管理ツールの機能（スプリント・ボード・見積り・依存関係）。**案件を受けて記録して見せるところまで**
- 一般的な監視・APM・トレース。自動検知は「アプリが送ってきたもの」を受けるだけで、こちらから測りに行かない
- SaaS 提供。自己ホスト前提

## 技術スタック

| 層 | 採用 | 備考 |
|---|---|---|
| すべて | **未定** | 設計が先。実装言語・DB・配布形態はこれから決める |

## ディレクトリ構成

| パス | 中身 |
|---|---|
| `README.md` | 何を解くものかの説明（公開向け） |
| `scripts/` | secrets-scan と CLAUDE.md 構造検査 |
| `docs/local/` | 作業ノート。**追跡しない**（Drive 同期領域へのジャンクション） |

## 主要コマンド

- secrets-scan（手動）: `node scripts/secrets-scan.mjs --staged --block`
- CLAUDE.md 構造検査: `node scripts/check-claude-md.mjs`

## 設計原則の索引（本文は正本にある）

事故から生まれた設計ルールを追記する表。**本文はここに書かず、破る人が必ず開く場所に置く。**

| ルール | 正本（本文はここ） | 機械検査 |
|---|---|---|
| 送れなかったときに、送ったことにしない | `docs/local/plan_central-case-ledger.md`（設計が固まったら公開側の設計文書へ移す） | なし（実装時に受入テストへ） |
| 業務固有の語彙をコードへ書かない（設定へ逃がす） | `scripts/secrets-scan.mjs` | pre-commit / CI |

**新しいルールを足す前に、まずこの表に 1 行足せる形にできないかを考える。** できないもの
（機械検査も、決まったファイルも無いもの）だけが本文を持ってよい。

## AI 作業共通ルール

ビルド・コミット禁止、secrets-scan 責務、plan/bugfix/pending md の作成ルール等の AI 作業共通ルールは、各利用者のグローバル AI 設定に従う（作者環境の例: `~/.claude/CLAUDE.md` および `~/.claude/guides/`）。

このリポジトリ固有:

- **これは public リポジトリ。** 実運用の接続先・組織名・システム名・人名・識別番号を、
  追跡対象ファイルへ 1 文字も書かない。作業ノートは `docs/local/`（追跡外）へ
- **業務固有の語彙は設定へ逃がす。** 案件の種別や区分の値をコードへ直接書かない。
  設定で定義し、サンプルだけを公開する
- **設計が固まるまで実装を始めない。** 表の形と受け口の契約が決まっていない状態でコードを書くと、
  後から契約を合わせる作業が全アプリに波及する

## Obsidian artifacts

If `docs/obsidian/README.md` exists, use it as an index for related knowledge artifacts.
Use the repository-relative `docs/obsidian` entry. Do not write to a central absolute
path and do not silently fall back to `docs/local` when the entry is missing.

## secrets-scan（このリポジトリの配線）

書く瞬間の責務（固有名詞の一般化・fixture は合成データ等）は上記「AI 作業共通ルール」の参照先に従う。このリポジトリ固有の配線は以下:

- scanner: `scripts/secrets-scan.mjs`（手動実行: `node scripts/secrets-scan.mjs --staged --block`）
- layer 2: `.githooks/pre-commit`（`core.hooksPath = .githooks`。clone 直後は `bash scripts/install-hooks.sh` または `pwsh scripts/install-hooks.ps1` で有効化）
- layer 3: `.github/workflows/secrets-scan.yml`
- **hook には `check-claude-md.mjs` も並べてある。各行に `|| exit 1` が付いていることを消さない**
  （付け忘れると前段の BLOCKED が握り潰されて commit が通る）
- env (full coverage に必要・未設定なら構造 regex のみで継続): `KB_ROOT` / `FAMILY_ROOT`。設定詳細は `scripts/secrets-scan.mjs` の冒頭コメント

## 関連ドキュメント

| 項目 | パス |
|---|---|
| ユーザー向け README | `README.md` |
| Codex/他 AI 用入口 | `AGENTS.md` |
| ローカル作業ノート（非公開） | `docs/local/` |
