-- issuepost — 初期スキーマ
--
-- 対象: MariaDB 10.6 以降（CHECK 制約と DYNAMIC 行フォーマットを前提にする）
-- 正本: docs/design.md の「1. 表の形」の項目表。この SQL はそれを落としたもので、
--       食い違ったら docs/design.md の項目表が正しい。
--
-- この SQL が守っている決めごと（理由は設計文書にある）:
--   * 台帳の外のテーブルへ外部キーを張らない。張るのは台帳の中だけ
--   * 種別・状態・承認状態は ENUM にしない（下の「なぜ ENUM にしないか」）
--   * 案件番号を発行するのは台帳だけ。アプリは発行しない
--   * 添付の実体は持たない。参照とメタだけ持ち、実体が消えても行は残す
--   * 案件は消さない。子表の外部キーを CASCADE にせず、消せない形にしてある
--   * 状態と承認の移り変わりは上書きせず、追記の表に残す
--
-- 日時はすべて UTC で保存する。サーバと全接続のタイムゾーンを UTC に固定すること。
-- これを守らないと「期限を過ぎた」の境界が、見る場所によって 1 日ずれる。

SET NAMES utf8mb4;

-- ---------------------------------------------------------------------------
-- なぜ種別・状態・承認状態を ENUM にしないか
--
-- これらは組織ごとに変わる値で、設定で定義することにしてある。ENUM にすると
-- 業務固有の語がスキーマに焼き付き、値を 1 つ増やすたびに ALTER TABLE になる。
-- ここでは VARCHAR で受け、取り込む側が設定の一覧に対して検査する。
-- 一覧に無い値は受け口で弾く（設計文書 §3）。
--
-- 第三の案として「設定の値を起動時に流し込む参照表を作り、外部キーで縛る」も検討した。
-- DB 側で担保できる利点はあるが、設定ファイルと参照表という同じ一覧の写しが 2 か所になり、
-- どちらが正かという問題が戻る。設定を正本の 1 か所に保つほうを採った。
--
-- 逆に origin / link_type / deleted_reason / event_type は ENUM にしてある。こちらは
-- 設計が決めた値で、組織ごとに変わらない。増えるときは設計が変わるときなので、
-- ALTER TABLE になるのが正しい。
--
-- 識別子として扱う列は *_bin の照合順序にしてある。既定の utf8mb4_unicode_ci は
-- 大文字小文字を区別しないので、`U-10482` と `u-10482` が同じ人になり、
-- 人数の集計が狂う。識別子は「文字列として同じかどうか」で比べる。
-- ---------------------------------------------------------------------------

-- 案件番号の採番。source ごとに 1 行だけ持つ。
--
-- 採番は次の形で行う（同じトランザクションの中で）:
--   INSERT INTO case_number_sequences (source, next_seq) VALUES (?, 2)
--     ON DUPLICATE KEY UPDATE next_seq = LAST_INSERT_ID(next_seq) + 1;
-- 払い出した番号は、影響行数で場合分けして決める。
--   影響行数 1 = 行を作った（新しい source）→ 払い出した番号は 1
--   影響行数 2 = 既存の行を更新した        → SELECT LAST_INSERT_ID() が現在値を返す
-- **新しい source のときに LAST_INSERT_ID() を読んではいけない。**
-- この表には AUTO_INCREMENT の列が無いので、素の INSERT は LAST_INSERT_ID() を
-- 更新しない。読むと接続に残っている前の値（新しい接続なら 0）が返る。
-- UPDATE だけにすると、行が無いときに 0 行ヒットでエラーも出ず、
-- LAST_INSERT_ID() がセッションの前の値を返して黙って番号が壊れる。
CREATE TABLE case_number_sequences (
  source    VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
            COMMENT 'アプリの識別子。番号の接頭辞になる',
  next_seq  INT UNSIGNED NOT NULL DEFAULT 1 COMMENT '次に払い出す連番',
  PRIMARY KEY (source)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='案件番号の採番。発行するのは台帳だけ';

-- 案件。1 件の不具合・要望・質問・メモ、またはアプリが検知した異常。
CREATE TABLE cases (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '台帳の中だけの連番。誰にも見せない',

  source          VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
                  COMMENT 'どのアプリから来たか',
  seq             INT UNSIGNED    NOT NULL COMMENT 'source の中での連番',
  number          VARCHAR(110) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
                  COMMENT '公開される案件番号。<source>-<seq> と一致することを CHECK で縛る',

  -- 1 本のアプリを複数の顧客が使う構成のための軸。
  -- 顧客で分かれていないアプリは空文字を入れる（設計文書 §4）。
  tenant_ref      VARCHAR(128) COLLATE utf8mb4_bin NOT NULL DEFAULT ''
                  COMMENT 'アプリ側が持っている顧客の識別子。外部キーは張らない',

  origin          ENUM('human','detected') NOT NULL COMMENT '人が報告 / アプリが検知',
  kind            VARCHAR(32)     NOT NULL COMMENT '設定の一覧に対して検査する',
  status          VARCHAR(32)     NOT NULL COMMENT '設定の一覧に対して検査する',
  approval_state  VARCHAR(32)     NOT NULL COMMENT '設定の一覧に対して検査する。既定値は置かない',

  title           VARCHAR(255)    NOT NULL,
  body            MEDIUMTEXT      NOT NULL COMMENT '受け取ったまま保管する。加工しない',

  -- 人が報告したときだけ入る。アプリが検知した異常には報告者がいないので NULL。
  reporter_ref    VARCHAR(128) COLLATE utf8mb4_bin NULL
                  COMMENT 'アプリが持っている識別子。外部キーは張らない',

  screen_id       VARCHAR(128)    NOT NULL DEFAULT '' COMMENT 'アプリが付ける。報告者に選ばせない',
  feature_id      VARCHAR(128)    NOT NULL DEFAULT '',
  environment     VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '番号には入れない',
  version         VARCHAR(64)     NOT NULL DEFAULT '',
  url             VARCHAR(1024)   NOT NULL DEFAULT '',

  fingerprint     CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL
                  COMMENT 'origin=detected のときだけ入る',

  promised_due    DATE            NULL COMMENT '相手に約束した期日。「期限を過ぎた」はこれだけを指す',
  hold_until      DATE            NULL COMMENT '保留の解除期限',
  closed_at       DATETIME        NULL COMMENT '終端状態になった日時。添付の期限の起点',

  duplicate_of    BIGINT UNSIGNED NULL COMMENT 'まとめた先の案件',

  -- アプリ側が持っていた元の識別子。送ってくれば (source, legacy_ref) が一意になり、
  -- 同じものを 2 回送っても 2 件にならない（受け口の冪等性）。
  -- 送らないときは NULL。MariaDB の UNIQUE は NULL 同士を衝突させないので、
  -- 「付けた送り主だけが冪等になる」形で共存できる。
  legacy_ref      VARCHAR(128) COLLATE utf8mb4_bin NULL
                  COMMENT 'アプリ側の元 ID。付ければ二重登録を防げる',

  created_at      DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

  PRIMARY KEY (id),
  -- 番号は世界で一意。コミットメッセージとの文字列一致がこれに乗っている
  UNIQUE KEY uq_cases_number (number),
  -- 採番の取りこぼし・二重払い出しを DB 側でも防ぐ
  UNIQUE KEY uq_cases_source_seq (source, seq),
  -- 受け口の冪等性。legacy_ref が NULL の行は衝突しない
  UNIQUE KEY uq_cases_legacy (source, legacy_ref),

  -- 顧客側の一覧（合言葉が source と tenant_ref を固定する。設計文書 §4）
  KEY ix_cases_t_status  (source, tenant_ref, status, promised_due),
  KEY ix_cases_t_screen  (source, tenant_ref, screen_id),
  KEY ix_cases_t_kind    (source, tenant_ref, kind, created_at),
  KEY ix_cases_t_person  (source, tenant_ref, reporter_ref),
  -- 運営側の横断一覧（source をまたぐので、先頭に source を置かない）
  KEY ix_cases_x_status  (status, promised_due),
  KEY ix_cases_x_kind    (kind, created_at),
  KEY ix_cases_x_screen  (screen_id),
  -- そのほか
  KEY ix_cases_fingerprint  (source, fingerprint),
  KEY ix_cases_hold         (hold_until),
  KEY ix_cases_closed       (closed_at),
  KEY ix_cases_duplicate_of (duplicate_of),

  CONSTRAINT fk_cases_duplicate_of FOREIGN KEY (duplicate_of) REFERENCES cases (id),

  -- number が source と seq からずれないようにする。
  -- 3 つを別々に持つ以上、一致は人ではなく DB に守らせる
  CONSTRAINT ck_cases_number CHECK (number = CONCAT(source, '-', seq)),
  -- 検知した異常には指紋があり報告者がいない。人の報告はその逆
  CONSTRAINT ck_cases_detected CHECK (
    (origin = 'detected' AND fingerprint IS NOT NULL AND reporter_ref IS NULL) OR
    (origin = 'human'    AND fingerprint IS NULL     AND reporter_ref IS NOT NULL)
  )
  -- 保留の期限（hold_until）は、承認が保留のときだけ入る。
  -- **これは CHECK にしていない。**承認状態の値は設定で定義するもので
  -- （この表の上の「なぜ ENUM にしないか」）、どの語が保留かをスキーマが名指しすると、
  -- 組織が語を差し替えた瞬間に、正しい行が入らなくなる。
  -- 対にすることは受け口が守る（設定の approval_states.hold）。
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='案件。4 種類を 1 つの表で扱う';

-- 同じことに当たった人。2 件目以降の報告は新しい案件を作らず、ここに 1 行足す。
-- この件数が優先順位の根拠になる。人の報告にだけ付く。
CREATE TABLE case_people (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  case_id       BIGINT UNSIGNED NOT NULL,
  reporter_ref  VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT 'アプリが持っている識別子',
  reported_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,

  PRIMARY KEY (id),
  -- 同じ人が 2 回言っても 2 人に数えない。人数が根拠になる以上、ここは厳密に
  UNIQUE KEY uq_case_people (case_id, reporter_ref),

  CONSTRAINT fk_case_people_case FOREIGN KEY (case_id) REFERENCES cases (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='同じことに当たった人。人数が優先順位の根拠になる';

-- 状態と承認の移り変わり。追記だけで、更新も削除もしない。
--
-- cases.status と cases.approval_state は「いまの値」で、上書きされる。
-- 上書きすると経緯が消えるので、変わるたびにここへ 1 行残す。
-- 「4 本ぶんの経緯がここに集まる」と言う以上、経緯そのものを消さない。
CREATE TABLE case_events (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  case_id     BIGINT UNSIGNED NOT NULL,
  event_type  ENUM('status','approval') NOT NULL,
  from_value  VARCHAR(32)     NULL COMMENT '最初の 1 行は NULL',
  to_value    VARCHAR(32)     NOT NULL,
  note        VARCHAR(1024)   NOT NULL DEFAULT '' COMMENT '却下の理由など',
  actor_ref   VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT '変えた人。アプリ側の識別子',
  occurred_at DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,

  PRIMARY KEY (id),
  KEY ix_case_events_case (case_id, occurred_at),

  CONSTRAINT fk_case_events_case FOREIGN KEY (case_id) REFERENCES cases (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='状態と承認の移り変わり。追記だけ';

-- 報告した人へ返したもの。
CREATE TABLE case_replies (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  case_id      BIGINT UNSIGNED NOT NULL,
  body         MEDIUMTEXT      NOT NULL,
  author_ref   VARCHAR(128) COLLATE utf8mb4_bin NOT NULL COMMENT '返した人。アプリ側の識別子',
  replied_at   DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  -- 台帳に保存できたことと、相手に届いたことは別。
  -- 案件について「送れなかったら送ったことにしない」と決めた以上、返信も同じ扱いにする
  delivered_at DATETIME        NULL COMMENT '通知経路が受け取った日時。届いていなければ空',

  PRIMARY KEY (id),
  KEY ix_case_replies_case (case_id, replied_at),
  KEY ix_case_replies_undelivered (delivered_at),

  CONSTRAINT fk_case_replies_case FOREIGN KEY (case_id) REFERENCES cases (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='報告した人へ返したもの';

-- 案件に紐づくもの。コミット・文書・外部 URL。
--
-- コミットは、リポジトリの複製を持っている側（開発者の手元、またはビルドの仕組み）が
-- 案件と同じ受け口へ送ってくる。台帳はそれをここへ保存する。
-- 台帳はリポジトリを複製しないし、バージョン管理を実行しない（設計文書 §3）。
--
-- 同じコミットが 2 回送られてきても 1 行にしかならないことを、UNIQUE で担保する。
CREATE TABLE case_links (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  case_id     BIGINT UNSIGNED NOT NULL,
  link_type   ENUM('commit','doc','url') NOT NULL,
  ref         VARCHAR(512) COLLATE utf8mb4_bin NOT NULL,
  created_at  DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,

  PRIMARY KEY (id),
  UNIQUE KEY uq_case_links (case_id, link_type, ref),
  KEY ix_case_links_case (case_id),

  CONSTRAINT fk_case_links_case FOREIGN KEY (case_id) REFERENCES cases (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='案件に紐づくもの。直した証拠がここにぶら下がる';

-- 添付。実体は外部のファイル置き場にあり、ここには参照とメタだけ持つ。
-- 実体が消えても行は消さない（設計文書 §6）。
CREATE TABLE case_attachments (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  case_id         BIGINT UNSIGNED NOT NULL,

  ref             VARCHAR(512) COLLATE utf8mb4_bin NOT NULL
                  COMMENT '置き場での参照。実体が消えても空にしない',
  filename        VARCHAR(255)    NOT NULL COMMENT '元のファイル名',
  mime            VARCHAR(127)    NOT NULL,
  size_bytes      BIGINT UNSIGNED NOT NULL,
  sha256          CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
                  COMMENT '台帳が受け取ったバイト列から計算する。実体が消えても残す',

  created_at      DATETIME        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at      DATETIME        NULL COMMENT '実体が無くなった日時',
  deleted_reason  ENUM('expired','removed_by_user','missing_at_store') NULL,

  PRIMARY KEY (id),
  -- 期限切れの抽出は cases 側（closed_at）から入り、case_id で引く
  KEY ix_case_attachments_case (case_id, deleted_at),
  -- 同じものが後から来たときの判定。まだ実体がある行だけを見る
  KEY ix_case_attachments_sha  (sha256, deleted_at),

  CONSTRAINT fk_case_attachments_case FOREIGN KEY (case_id) REFERENCES cases (id),
  -- 理由なしで削除済みの印を付けられないようにする。
  -- 日時だけ入れて理由を空にすると、後から「なぜ消えたのか」に答えられなくなる
  CONSTRAINT ck_case_attachments_deleted CHECK (
    (deleted_at IS NULL     AND deleted_reason IS NULL) OR
    (deleted_at IS NOT NULL AND deleted_reason IS NOT NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='添付の参照とメタ。実体は持たない';

-- 定期実行の記録。
--
-- 受入条件に「定期実行が実際に登録され、動いていることを確認した」がある。
-- 確認するには、確認できるものが要る。仕組みを作っただけでは動かないので、
-- 「最後にいつ走って、何件処理したか」を見られるようにしておく。
CREATE TABLE cleanup_runs (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  started_at    DATETIME        NOT NULL,
  finished_at   DATETIME        NULL COMMENT '落ちたときは空のまま残る',
  dry_run       TINYINT(1)      NOT NULL DEFAULT 0,
  -- 空実行と本実行で、同じ名前の数字が同じ意味になるようにする。
  -- 片方が「消す予定の数」でもう片方が「消えた数」だと、ずれても誰も気づかない
  targeted      INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '対象と判定した件数',
  deleted       INT UNSIGNED    NOT NULL DEFAULT 0 COMMENT '実際に消えた件数。空実行では 0',
  failed        INT UNSIGNED    NOT NULL DEFAULT 0,
  bytes_freed   BIGINT UNSIGNED NOT NULL DEFAULT 0,

  PRIMARY KEY (id),
  KEY ix_cleanup_runs_started (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  ROW_FORMAT=DYNAMIC
  COMMENT='定期実行の記録。動いている根拠になる';

-- ---------------------------------------------------------------------------
-- この表が答えられること（設計文書 §5 の 4 枚のパネルに対応する）
--
-- 顧客側の画面は、合言葉が source と tenant_ref を固定する。
-- 運営側の横断画面だけが source をまたぐ。絞るのは画面ではなく受け口（設計文書 §4）。
--
--   期限を過ぎた       : promised_due < CURDATE() AND status が open  → ix_cases_t_status
--   誰待ちで止まっている: status 別の件数                              → ix_cases_t_status
--   どの画面で起きている: screen_id 別の件数                           → ix_cases_t_screen
--   種類の比率         : kind 別の件数                                → ix_cases_t_kind
--
-- 運営側の同じ 4 枚は ix_cases_x_* を使う。先頭に source が無いのは、
-- source をまたいで数えるため（先頭が source の索引は、source を固定しないと使えない）。
--
-- 「期限を過ぎた」を期日順に並べると、status の IN が複数レンジになるので filesort になる。
-- 件数が増えたら別途手を打つ。いまは許容する。
--
-- 添付の期限切れの抽出:
--   SELECT a.* FROM cases c
--     JOIN case_attachments a ON a.case_id = c.id AND a.deleted_at IS NULL
--    WHERE c.closed_at IS NOT NULL
--      AND c.closed_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL ? DAY)
--    ORDER BY c.closed_at, a.id
--    LIMIT ?;
-- ORDER BY を付けるのは、batch_limit で切り出す順序を決めるため。
-- 順序が不定だと、上限と実行間隔の関係を検証するときに数えられない。
-- ---------------------------------------------------------------------------
