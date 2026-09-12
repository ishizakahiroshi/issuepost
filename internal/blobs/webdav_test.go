package blobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// テストの fixture はすべて合成データ。

func newTestDAV(t *testing.T, h http.HandlerFunc) (*WebDAV, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := New(Config{BaseURL: srv.URL + "/box", User: "u", Password: "p"})
	if err != nil {
		t.Fatalf("置き場の口を作れない: %v", err)
	}
	return d, srv
}

// 置いたら、格納フォルダからの相対の位置を返す。**絶対 URL を保存しない。**
func TestPutReturnsRelativeRef(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody []byte
	d, _ := newTestDAV(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = buf
		w.WriteHeader(http.StatusCreated)
	})

	ref, err := d.Put(context.Background(), "app_one-1-abc-画面.png", []byte("bytes"))
	if err != nil {
		t.Fatalf("置けない: %v", err)
	}
	if ref != "app_one-1-abc-画面.png" {
		t.Errorf("参照が違う: %q", ref)
	}
	if gotPath != "/box/app_one-1-abc-画面.png" {
		t.Errorf("置いた先が違う: %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Error("認証を付けていない")
	}
	if string(gotBody) != "bytes" {
		t.Errorf("送ったバイト列が違う: %q", gotBody)
	}
}

// 消しに行って無かったことは、失敗ではなく 1 つの結果。
func TestRemoveMissingIsNotFound(t *testing.T) {
	d, _ := newTestDAV(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if err := d.Remove(context.Background(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("無いことが伝わらない: %v", err)
	}
}

// **転送は追わない。**追った先は、問い合わせた相手とは限らない。
func TestPutDoesNotFollowRedirect(t *testing.T) {
	hops := 0
	d, _ := newTestDAV(t, func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	if _, err := d.Put(context.Background(), "a.bin", []byte("x")); err == nil {
		t.Fatal("転送を成功として扱っている")
	}
	if hops != 1 {
		t.Errorf("転送を追っている: %d 回", hops)
	}
}

// 置き場を書いていない設定は通す（添付を使わない構成）。書いたなら足りない項目で止める。
func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err != nil {
		t.Errorf("書いていない設定で止まっている: %v", err)
	}
	for _, c := range []Config{
		{BaseURL: "http://files.example.com/box", User: "u"}, // 平文
		{BaseURL: "https://files.example.com/box"},           // 利用者名が無い
		{BaseURL: "not a url", User: "u"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("通してはいけない設定が通った: %+v", c)
		}
	}
	if err := (Config{BaseURL: "https://files.example.com/box", User: "u"}).Validate(); err != nil {
		t.Errorf("正しい設定で止まっている: %v", err)
	}
}
