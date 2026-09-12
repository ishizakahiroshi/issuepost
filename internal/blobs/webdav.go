// Package blobs は添付の実体の置き場。
//
// **台帳は実体を持たない**（設計文書 §1）。持っているのは参照とメタだけで、
// バイト列は外のファイル置き場（WebDAV）へ置く。この package はそこへ
// 置く・消すの 2 つだけを行う。
//
// **転送（リダイレクト）は追わない。**追うと、置いたつもりの先と実際の先が
// 食い違い、後から参照を検証する手段が無くなる（設計文書 §6 の最後の節）。
package blobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound は置き場にそのものが無い。
//
// **消しに行って無かったことは、失敗ではなく 1 つの結果。**置き場は台帳の
// 知らないところで消えることがあるので、呼ぶ側はこれを見て理由を分ける（設計文書 §6）。
var ErrNotFound = errors.New("blobs: 置き場に無い")

// Config は置き場への繋ぎ方。
type Config struct {
	// BaseURL は置き場の格納フォルダ。空なら添付の口は動かない。
	BaseURL string `toml:"base_url"`
	// User / Password は置き場の専用アカウント。**ログにも応答にも出さない。**
	User     string `toml:"user"`
	Password string `toml:"password"`
	// TimeoutSeconds は 1 回の置き・消しの待ち時間。0 なら既定値。
	TimeoutSeconds int `toml:"timeout_seconds"`
}

const defaultTimeoutSeconds = 30

// Configured は置き場が設定されているかを返す。
func (c Config) Configured() bool { return strings.TrimSpace(c.BaseURL) != "" }

// Validate は繋ぎ方の設定を検証する。**書いていないことは許す**（添付を使わない構成）。
// 書いたなら、足りない項目があるまま起動しない。
func (c Config) Validate() error {
	if !c.Configured() {
		return nil
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("attachments.base_url が URL として読めない")
	}
	if u.Scheme != "https" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		// 鍵付きのファイルが平文で流れる形を既定にしない。
		// 同じホストの中だけは、前段を挟まない構成があるので許す。
		return fmt.Errorf("attachments.base_url は https にする")
	}
	if strings.TrimSpace(c.User) == "" {
		return fmt.Errorf("attachments.user が空")
	}
	if c.TimeoutSeconds < 0 {
		return fmt.Errorf("attachments.timeout_seconds が負")
	}
	return nil
}

// WebDAV は WebDAV の置き場。
type WebDAV struct {
	base *url.URL
	cfg  Config
	cl   *http.Client
}

// New は置き場への口を作る。[Config.Validate] を通した設定を渡す。
func New(cfg Config) (*WebDAV, error) {
	base, err := url.Parse(strings.TrimSuffix(cfg.BaseURL, "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("blobs: base_url を読めない")
	}
	timeout := cfg.TimeoutSeconds
	if timeout == 0 {
		timeout = defaultTimeoutSeconds
	}
	return &WebDAV{
		base: base,
		cfg:  cfg,
		cl: &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
			// 転送を追わない。追った先は、問い合わせた相手とは限らない。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Put はバイト列を置き、参照を返す。参照は格納フォルダからの相対の位置。
//
// **絶対 URL を保存しない。**置き場の入口が変わったときに、保存済みの参照が
// 全部死ぬ形にしないため。
func (d *WebDAV) Put(ctx context.Context, name string, data []byte) (string, error) {
	ref := strings.TrimPrefix(name, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.resolve(ref), bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("blobs: 置く要求を作れない")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))
	res, err := d.do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return ref, nil
	default:
		// 応答の本文をエラーに載せない。置き場の応答に何が入っているか分からない。
		return "", fmt.Errorf("blobs: 置けなかった（%d）", res.StatusCode)
	}
}

// Remove は実体を消す。無ければ [ErrNotFound]。
func (d *WebDAV) Remove(ctx context.Context, ref string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, d.resolve(ref), nil)
	if err != nil {
		return fmt.Errorf("blobs: 消す要求を作れない")
	}
	res, err := d.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted:
		return nil
	case http.StatusNotFound, http.StatusGone:
		return ErrNotFound
	default:
		return fmt.Errorf("blobs: 消せなかった（%d）", res.StatusCode)
	}
}

func (d *WebDAV) do(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(d.cfg.User, d.cfg.Password)
	res, err := d.cl.Do(req)
	if err != nil {
		// エラー文に URL を載せない（利用者名を含む形で書かれることがある）。
		return nil, fmt.Errorf("blobs: 置き場へ届かない")
	}
	return res, nil
}

func (d *WebDAV) resolve(ref string) string {
	return d.base.String() + (&url.URL{Path: ref}).EscapedPath()
}
