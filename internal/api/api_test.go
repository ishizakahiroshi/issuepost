package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ishizakahiroshi/doorpost/oplog"
	"github.com/ishizakahiroshi/doorpost/respond"

	"github.com/ishizakahiroshi/issuepost/internal/blobs"
	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// テストの fixture はすべて合成データ。
// IP は RFC 5737 のドキュメント用。httptest.NewRequest の既定の RemoteAddr が
// 192.0.2.1 なので、許可 IP をそれに合わせてある。
const (
	keyAppOne  = "key-for-test----------------------------"
	keyConsole = "another-key-for-test--------------------"
	reporter   = "u-10482"
	// 64 桁の 16 進。検知した異常の指紋の形。
	fingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// fakeLedger は保存先の代わり。何を渡されたかを覚えて、決めた結果を返す。
type fakeLedger struct {
	mu      sync.Mutex
	calls   int
	last    store.NewCase
	created store.Created
	err     error

	// 読み出しの側。渡された絞り込みを覚えて、決めた結果を返す。
	lastFilter store.Filter
	lastPage   store.Page
	list       []store.Case
	item       store.Case
	itemFound  bool

	// 書き換えの側。渡されたものを覚える。
	lastChanges store.Changes
	lastPerson  string
	lastReply   [2]string // body, author_ref
	lastLink    [2]string // type, ref
	lastAttach  store.NewAttachment
	lastMark    [2]string // id, reason
	personAdded bool
	linkAdded   bool
	attachment  store.Attachment
	attachFound bool
	used        uint64
}

func (f *fakeLedger) UpdateCase(_ context.Context, filter store.Filter, _ string, ch store.Changes) (store.Case, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastChanges = ch
	if f.err != nil {
		return store.Case{}, false, f.err
	}
	return f.item, f.itemFound, nil
}

func (f *fakeLedger) AddPerson(_ context.Context, filter store.Filter, _, reporterRef string) (bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastPerson = reporterRef
	if f.err != nil {
		return false, false, f.err
	}
	return f.personAdded, f.itemFound, nil
}

func (f *fakeLedger) AddReply(_ context.Context, filter store.Filter, _, body, authorRef string) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastReply = [2]string{body, authorRef}
	if f.err != nil {
		return 0, false, f.err
	}
	return 7, f.itemFound, nil
}

func (f *fakeLedger) AddLink(_ context.Context, filter store.Filter, _, linkType, ref string) (uint64, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastLink = [2]string{linkType, ref}
	if f.err != nil {
		return 0, false, false, f.err
	}
	return 9, f.linkAdded, f.itemFound, nil
}

func (f *fakeLedger) AddAttachment(_ context.Context, filter store.Filter, _ string, a store.NewAttachment) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastAttach = a
	if f.err != nil {
		return 0, false, f.err
	}
	return 11, f.itemFound, nil
}

func (f *fakeLedger) FindAttachment(_ context.Context, filter store.Filter, _ string, _ uint64) (store.Attachment, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	if f.err != nil {
		return store.Attachment{}, false, f.err
	}
	return f.attachment, f.attachFound, nil
}

func (f *fakeLedger) MarkAttachmentDeleted(_ context.Context, id uint64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMark = [2]string{strconv.FormatUint(id, 10), reason}
	return nil
}

func (f *fakeLedger) UsedBytes(_ context.Context) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.used, nil
}

// fakeFiles は添付の置き場の代わり。置いたものを覚える。
type fakeFiles struct {
	mu        sync.Mutex
	putName   string
	putData   []byte
	putErr    error
	removed   []string
	removeErr error
}

func (f *fakeFiles) Put(_ context.Context, name string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return "", f.putErr
	}
	f.putName = name
	f.putData = data
	return name, nil
}

func (f *fakeFiles) Remove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, ref)
	return f.removeErr
}

func (f *fakeFiles) snapshot() (string, []byte, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putName, f.putData, f.removed
}

func (f *fakeLedger) ListCases(_ context.Context, filter store.Filter, p store.Page) ([]store.Case, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	f.lastPage = p
	if f.err != nil {
		return nil, f.err
	}
	return f.list, nil
}

func (f *fakeLedger) GetCase(_ context.Context, filter store.Filter, _ string) (store.Case, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastFilter = filter
	if f.err != nil {
		return store.Case{}, false, f.err
	}
	return f.item, f.itemFound, nil
}

func (f *fakeLedger) filterSnapshot() store.Filter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastFilter
}

func (f *fakeLedger) CreateCase(_ context.Context, c store.NewCase) (store.Created, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = c
	if f.err != nil {
		return store.Created{}, f.err
	}
	got := f.created
	if got.Number == "" {
		got = store.Created{Number: c.Source + "-234", Status: c.Status}
	}
	return got, nil
}

func (f *fakeLedger) snapshot() (int, store.NewCase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.last
}

type harness struct {
	handler http.Handler
	ledger  *fakeLedger
	files   *fakeFiles
	logs    *bytes.Buffer
	cfg     *Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, exampleTOML)
}

func newHarnessWith(t *testing.T, toml string) *harness {
	t.Helper()
	cfg, err := decode(t, toml)
	if err != nil {
		t.Fatalf("設定が読めない: %v", err)
	}
	led := &fakeLedger{}
	files := &fakeFiles{}
	logs := &bytes.Buffer{}
	srv := New(cfg, led, files, oplog.New(logs))
	return &harness{handler: srv.Handler(), ledger: led, files: files, logs: logs, cfg: cfg}
}

// attachTOML は添付の置き場を設定した版。実値は 1 つも入っていない。
const attachTOML = `
[attachments]
base_url = "https://files.example.com/issuepost"
user     = "issuepost"
password = "synthetic-password-for-test"
max_file_bytes    = 1024
total_limit_bytes = 4096
`

// newAttachHarness は添付の口が動く形。
func newAttachHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, exampleTOML+attachTOML)
}

// send は本文付きの 1 要求。
func (h *harness) send(method, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// upload は multipart で 1 件送る。
func (h *harness) upload(key, path, filename, mimeType string, data []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	head := make(textproto.MIMEHeader)
	head.Set("Content-Disposition",
		`form-data; name="file"; filename="`+filename+`"`)
	if mimeType != "" {
		head.Set("Content-Type", mimeType)
	}
	part, err := mw.CreatePart(head)
	if err != nil {
		panic(err)
	}
	if _, err := part.Write(data); err != nil {
		panic(err)
	}
	if err := mw.Close(); err != nil {
		panic(err)
	}

	r := httptest.NewRequest(http.MethodPost, path, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// post は 1 件送る。body は JSON の文字列そのもの。
func (h *harness) post(key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/cases", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// get は読み出しの口を叩く。path は問い合わせ文字列まで含めた形。
func (h *harness) get(key, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// humanCase は人が報告した 1 件の本文を組み立てる。
func humanCase(extra string) string {
	base := `"origin":"human","kind":"bug","title":"月次の画面が保存できない",
	  "body":"保存を押すとページの先頭に戻る。","reporter_ref":"` + reporter + `",
	  "place":{"screen_id":"monthly-sheet","feature_id":"save","environment":"production","version":"3.2.1","url":"https://app.example.com/sheets/monthly"}`
	if extra != "" {
		base += "," + extra
	}
	return "{" + base + "}"
}

func decodeResponse(t *testing.T, w *httptest.ResponseRecorder) caseResponse {
	t.Helper()
	var got caseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	return got
}

func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code respond.Code) {
	t.Helper()
	if w.Code != status {
		t.Errorf("status が違う: got %d want %d (%s)", w.Code, status, w.Body.String())
	}
	var got respond.ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	if got.Error != code {
		t.Errorf("エラーの語が違う: got %q want %q", got.Error, code)
	}
	if got.OK {
		t.Error("エラーなのに ok が true")
	}
}

// --- 受け取れる場合 -----------------------------------------------------------

// 人の報告を 1 件受け、台帳が発行した番号を返す。
func TestAcceptsHumanCase(t *testing.T) {
	h := newHarness(t)
	w := h.post(keyAppOne, humanCase(`"legacy_ref":"1042","promised_due":"2026-10-01"`))

	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	got := decodeResponse(t, w)
	if got.Number != "app_one-234" || got.Status != "new" {
		t.Errorf("応答が違う: %+v", got)
	}

	calls, saved := h.ledger.snapshot()
	if calls != 1 {
		t.Fatalf("保存の呼び出し回数が違う: %d", calls)
	}
	// source は本文ではなく合言葉から決まる。
	if saved.Source != "app_one" {
		t.Errorf("source が合言葉から決まっていない: %q", saved.Source)
	}
	if saved.TenantRef != "tenant_a" {
		t.Errorf("tenant が範囲から決まっていない: %q", saved.TenantRef)
	}
	if saved.Status != "new" || saved.ApprovalState != "not_required" {
		t.Errorf("初期の状態が違う: %q %q", saved.Status, saved.ApprovalState)
	}
	if saved.Fingerprint != "" {
		t.Errorf("人の報告に指紋が入っている: %q", saved.Fingerprint)
	}
	if saved.LegacyRef != "1042" || saved.PromisedDue != "2026-10-01" {
		t.Errorf("元 ID か期日が落ちている: %+v", saved)
	}
}

// 承認が要る種別は、承認待ちで入る。
func TestApprovalStateFollowsKind(t *testing.T) {
	h := newHarness(t)
	body := strings.Replace(humanCase(""), `"kind":"bug"`, `"kind":"request"`, 1)
	if w := h.post(keyAppOne, body); w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	_, saved := h.ledger.snapshot()
	if saved.ApprovalState != "pending" {
		t.Errorf("承認待ちで入っていない: %q", saved.ApprovalState)
	}
}

// アプリが検知した異常も同じ 1 本で受ける。報告者はいない。
func TestAcceptsDetectedCase(t *testing.T) {
	h := newHarness(t)
	body := `{"origin":"detected","kind":"bug","title":"保存が 500 を返している",
	  "body":"save handler が null 参照で落ちた。","fingerprint":"` + fingerprint + `",
	  "place":{"screen_id":"monthly-sheet","environment":"production"}}`
	w := h.post(keyAppOne, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	_, saved := h.ledger.snapshot()
	if saved.Origin != "detected" || saved.Fingerprint != fingerprint {
		t.Errorf("検知の項目が落ちている: %+v", saved)
	}
	if saved.ReporterRef != "" {
		t.Errorf("検知に報告者が入っている: %q", saved.ReporterRef)
	}
}

// 同じものを 2 回送っても 2 件にならない。既にある番号がそのまま返る。
func TestDuplicateReturnsExistingNumber(t *testing.T) {
	h := newHarness(t)
	h.ledger.created = store.Created{Number: "app_one-12", Status: "in_progress", Existed: true}

	w := h.post(keyAppOne, humanCase(`"legacy_ref":"1042"`))
	if w.Code != http.StatusOK {
		t.Fatalf("2 回目は 200 で返すはず: %d", w.Code)
	}
	got := decodeResponse(t, w)
	if got.Number != "app_one-12" || got.Status != "in_progress" {
		t.Errorf("既にある案件が返っていない: %+v", got)
	}
}

// --- 合言葉と見える範囲 -------------------------------------------------------

// 鍵が違えば通らない。
func TestRejectsWrongKey(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.post("wrong-key-------------------------------", humanCase("")),
		http.StatusUnauthorized, respond.InvalidKey)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("認証に落ちたのに保存が呼ばれた")
	}
}

// 鍵が無くても通らない。
func TestRejectsMissingKey(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.post("", humanCase("")), http.StatusUnauthorized, respond.InvalidKey)
}

// 鍵が合っていても、許可されていない IP からは通らない。
// **返る語は鍵違いと同じ。**区別して返すと、総当たりに当たり判定を与える。
func TestRejectsDisallowedIP(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/cases", strings.NewReader(humanCase("")))
	r.Header.Set("Authorization", "Bearer "+keyAppOne)
	r.RemoteAddr = "203.0.113.7:40000"
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	wantError(t, w, http.StatusUnauthorized, respond.InvalidKey)
}

// 範囲の外の顧客を名乗っても書けない。呼ぶ側の画面が絞ることを当てにしない。
func TestRejectsTenantOutOfScope(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.post(keyAppOne, humanCase(`"tenant_ref":"tenant_z"`)),
		http.StatusForbidden, errOutOfScope)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("範囲の外なのに保存が呼ばれた")
	}
}

// 範囲の中の顧客なら、本文が名乗ったものが入る。
func TestAcceptsTenantInScope(t *testing.T) {
	toml := strings.Replace(exampleTOML, `tenant = ["tenant_a"]`, `tenant = ["tenant_a", "tenant_b"]`, 1)
	h := newHarnessWith(t, toml)
	if w := h.post(keyAppOne, humanCase(`"tenant_ref":"tenant_b"`)); w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if _, saved := h.ledger.snapshot(); saved.TenantRef != "tenant_b" {
		t.Errorf("顧客が入っていない: %q", saved.TenantRef)
	}
}

// 顧客が複数ある範囲で、本文が書かなければどれか決められない。
func TestRejectsAmbiguousTenant(t *testing.T) {
	toml := strings.Replace(exampleTOML, `tenant = ["tenant_a"]`, `tenant = ["tenant_a", "tenant_b"]`, 1)
	h := newHarnessWith(t, toml)
	wantError(t, h.post(keyAppOne, humanCase("")), http.StatusBadRequest, respond.InvalidRequest)
}

// 顧客で分かれていないアプリは空のまま入る。
func TestTenantEmptyForWildcardScope(t *testing.T) {
	toml := strings.Replace(exampleTOML, `tenant = ["tenant_a"]`, `tenant = ["*"]`, 1)
	h := newHarnessWith(t, toml)
	if w := h.post(keyAppOne, humanCase("")); w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if _, saved := h.ledger.snapshot(); saved.TenantRef != "" {
		t.Errorf("顧客が空になっていない: %q", saved.TenantRef)
	}
}

// 横断で見る合言葉では案件を入れられない。
// 合言葉が source を固定することが、知らない接頭辞で採番されない理由そのもの。
func TestConsoleTokenCannotIntake(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.post(keyConsole, humanCase("")), http.StatusForbidden, errOutOfScope)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("横断の合言葉で保存が呼ばれた")
	}
}

// --- 本文の検査 ---------------------------------------------------------------

func TestRejectsBadRequests(t *testing.T) {
	longTitle := strings.Repeat("あ", maxTitleRunes+1)

	cases := []struct {
		name   string
		body   string
		status int
		code   respond.Code
	}{
		{"JSON として読めない", `{`, http.StatusBadRequest, respond.InvalidRequest},
		{
			"知らないフィールドがある",
			humanCase(`"number":"app_one-1"`),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"出どころが一覧に無い",
			strings.Replace(humanCase(""), `"origin":"human"`, `"origin":"imported"`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"種別が設定の一覧に無い",
			strings.Replace(humanCase(""), `"kind":"bug"`, `"kind":"defect"`, 1),
			http.StatusBadRequest, errUnknownKind,
		},
		{
			"題名が長すぎる",
			strings.Replace(humanCase(""), `"title":"月次の画面が保存できない"`, `"title":"`+longTitle+`"`, 1),
			http.StatusBadRequest, errTitleTooLong,
		},
		{
			"題名が空",
			strings.Replace(humanCase(""), `"title":"月次の画面が保存できない"`, `"title":""`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"本文が空",
			strings.Replace(humanCase(""), `"body":"保存を押すとページの先頭に戻る。"`, `"body":""`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"人の報告に報告者がいない",
			strings.Replace(humanCase(""), `"reporter_ref":"`+reporter+`"`, `"reporter_ref":""`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"人の報告に指紋が付いている",
			humanCase(`"fingerprint":"` + fingerprint + `"`),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"検知に報告者が付いている",
			`{"origin":"detected","kind":"bug","title":"t","body":"b","reporter_ref":"` + reporter + `","fingerprint":"` + fingerprint + `"}`,
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"検知に指紋が無い",
			`{"origin":"detected","kind":"bug","title":"t","body":"b"}`,
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"指紋の形が違う",
			`{"origin":"detected","kind":"bug","title":"t","body":"b","fingerprint":"not-a-fingerprint"}`,
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"環境名が設定に無い",
			strings.Replace(humanCase(""), `"environment":"production"`, `"environment":"sandbox"`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"期日が日付として読めない",
			humanCase(`"promised_due":"2026-13-45"`),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"元 ID が長すぎる",
			humanCase(`"legacy_ref":"` + strings.Repeat("x", maxRefRunes+1) + `"`),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"顧客の識別子が長すぎる",
			humanCase(`"tenant_ref":"` + strings.Repeat("x", maxRefRunes+1) + `"`),
			http.StatusBadRequest, respond.InvalidRequest,
		},
		{
			"報告者の識別子が長すぎる",
			strings.Replace(humanCase(""), `"reporter_ref":"`+reporter+`"`,
				`"reporter_ref":"`+strings.Repeat("x", maxRefRunes+1)+`"`, 1),
			http.StatusBadRequest, respond.InvalidRequest,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			wantError(t, h.post(keyAppOne, c.body), c.status, c.code)
			if calls, _ := h.ledger.snapshot(); calls != 0 {
				t.Error("検査に落ちたのに保存が呼ばれた")
			}
		})
	}
}

// 題名を切って通さない。切るのは加工で、送れなかったと伝えるほうがよい。
func TestTitleAtLimitPasses(t *testing.T) {
	h := newHarness(t)
	title := strings.Repeat("あ", maxTitleRunes)
	body := strings.Replace(humanCase(""), `"title":"月次の画面が保存できない"`, `"title":"`+title+`"`, 1)
	if w := h.post(keyAppOne, body); w.Code != http.StatusCreated {
		t.Fatalf("上限ちょうどは通るはず: %d (%s)", w.Code, w.Body.String())
	}
	if _, saved := h.ledger.snapshot(); saved.Title != title {
		t.Error("題名が切られている")
	}
}

// --- 保存できなかったとき -----------------------------------------------------

// 台帳へ書けなければ、番号を返さない。預からないし、送り直しもしない。
func TestLedgerFailureReturnsNoNumber(t *testing.T) {
	h := newHarness(t)
	h.ledger.err = fmt.Errorf("保存できない")

	w := h.post(keyAppOne, humanCase(""))
	wantError(t, w, http.StatusServiceUnavailable, errLedgerUnavailable)
	if strings.Contains(w.Body.String(), "number") {
		t.Errorf("保存できていないのに番号が出ている: %s", w.Body.String())
	}
}

// --- 読み出し -----------------------------------------------------------------

// 一覧は合言葉の範囲を必ず SQL へ渡す。**画面が絞ることを当てにしない。**
func TestListPassesScopeToLedger(t *testing.T) {
	h := newHarness(t)
	if w := h.get(keyAppOne, "/v1/cases"); w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	f := h.ledger.filterSnapshot()
	if len(f.Sources) != 1 || f.Sources[0] != "app_one" {
		t.Errorf("出どころの絞り込みが渡っていない: %+v", f.Sources)
	}
	if len(f.Tenants) != 1 || f.Tenants[0] != "tenant_a" {
		t.Errorf("顧客の絞り込みが渡っていない: %+v", f.Tenants)
	}
}

// 横断で見る合言葉だけが、絞り込みの無い一覧を引ける。
func TestListWildcardScopeHasNoFilter(t *testing.T) {
	h := newHarness(t)
	if w := h.get(keyConsole, "/v1/cases"); w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if f := h.ledger.filterSnapshot(); len(f.Sources) != 0 || len(f.Tenants) != 0 {
		t.Errorf("横断の合言葉に絞り込みが付いている: %+v", f)
	}
}

// 鍵が無ければ一覧も引けない。
func TestListRequiresKey(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.get("", "/v1/cases"), http.StatusUnauthorized, respond.InvalidKey)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("鍵が無いのに台帳が呼ばれた")
	}
}

// 知らない問い合わせ文字列は無視せず弾く。書いても効かないより、そこで気づけるほうがよい。
func TestListRejectsUnknownQuery(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.get(keyAppOne, "/v1/cases?status=new"), http.StatusBadRequest, respond.InvalidRequest)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("弾いたのに台帳が呼ばれた")
	}
}

// limit と cursor は数として読めなければ弾く。
func TestListRejectsBadPaging(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/cases?limit=0",
		"/v1/cases?limit=-1",
		"/v1/cases?limit=many",
		"/v1/cases?cursor=0",
		"/v1/cases?cursor=abc",
	} {
		wantError(t, h.get(keyAppOne, path), http.StatusBadRequest, respond.InvalidRequest)
	}
}

// 返した件数が上限と同じときだけ、続きの目印を付ける。
func TestListCursorOnlyWhenFull(t *testing.T) {
	h := newHarness(t)
	h.ledger.list = []store.Case{{ID: 41, Number: "app_one-41"}, {ID: 40, Number: "app_one-40"}}

	var full caseList
	w := h.get(keyAppOne, "/v1/cases?limit=2")
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	if full.NextCursor != "40" {
		t.Errorf("続きの目印が違う: %q", full.NextCursor)
	}

	var short caseList
	w = h.get(keyAppOne, "/v1/cases?limit=3")
	if err := json.Unmarshal(w.Body.Bytes(), &short); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	if short.NextCursor != "" {
		t.Errorf("続きが無いのに目印が付いている: %q", short.NextCursor)
	}
}

// 一覧に本文と内部の番号を出さない。
func TestListOmitsBodyAndInternalID(t *testing.T) {
	h := newHarness(t)
	h.ledger.list = []store.Case{{ID: 41, Number: "app_one-41", Title: "題名", Body: "本文がここにある"}}

	w := h.get(keyAppOne, "/v1/cases")
	out := w.Body.String()
	if strings.Contains(out, "本文がここにある") {
		t.Errorf("一覧に本文が出ている: %s", out)
	}
	if strings.Contains(out, `"id"`) || strings.Contains(out, "41,") {
		t.Errorf("一覧に内部の番号が出ている: %s", out)
	}
}

// 1 件は本文まで返す。日付は日付のまま、時刻を付けない。
func TestGetCaseReturnsBody(t *testing.T) {
	h := newHarness(t)
	due := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	h.ledger.item = store.Case{
		ID: 41, Number: "app_one-41", Source: "app_one", TenantRef: "tenant_a",
		Title: "題名", Body: "本文がここにある", PromisedDue: &due,
	}
	h.ledger.itemFound = true

	w := h.get(keyAppOne, "/v1/cases/app_one-41")
	if w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	var got caseItem
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	if got.Body != "本文がここにある" {
		t.Errorf("本文が返っていない: %q", got.Body)
	}
	if got.PromisedDue != "2026-10-01" {
		t.Errorf("期日の形が違う: %q", got.PromisedDue)
	}
	if f := h.ledger.filterSnapshot(); len(f.Sources) != 1 || f.Sources[0] != "app_one" {
		t.Errorf("1 件でも範囲が渡っていない: %+v", f)
	}
}

// 範囲の外は「無い」と同じに返す。「あるが見せない」と区別できると、
// 番号を順に当てるだけで他の顧客の案件の有無が分かる。
func TestGetCaseHidesOutOfScope(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = false
	wantError(t, h.get(keyAppOne, "/v1/cases/app_two-41"), http.StatusNotFound, errNotFound)
}

// 台帳へ届かないときは、無いことにしない。
func TestGetCaseLedgerFailure(t *testing.T) {
	h := newHarness(t)
	h.ledger.err = fmt.Errorf("引けない")
	wantError(t, h.get(keyAppOne, "/v1/cases/app_one-41"), http.StatusServiceUnavailable, errLedgerUnavailable)
}

// 1 件の口も POST は受けない。
func TestCaseItemRejectsOtherMethods(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/cases/app_one-41", strings.NewReader("{}"))
	r.Header.Set("Authorization", "Bearer "+keyAppOne)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	wantError(t, w, http.StatusMethodNotAllowed, respond.InvalidRequest)
}

// 下の口は POST だけ受ける。
func TestSubResourcesArePostOnly(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/cases/app_one-41/people",
		"/v1/cases/app_one-41/replies",
		"/v1/cases/app_one-41/links",
	} {
		wantError(t, h.get(keyAppOne, path), http.StatusMethodNotAllowed, respond.InvalidRequest)
	}
}

// 知らない名前の下の口は、method を見ずに「無い」で返す。
func TestUnknownSubResource(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.get(keyAppOne, "/v1/cases/app_one-41/history"), http.StatusNotFound, errNotFound)
}

// --- 変更（PATCH） ------------------------------------------------------------

func (h *harness) patch(body string) *httptest.ResponseRecorder {
	return h.send(http.MethodPatch, "/v1/cases/app_one-41", keyAppOne, body)
}

// 状態を変え、変更後の案件を返す。範囲は台帳へ渡る。
func TestPatchChangesStatus(t *testing.T) {
	h := newHarness(t)
	h.ledger.item = store.Case{Number: "app_one-41", Status: "done"}
	h.ledger.itemFound = true

	w := h.patch(`{"status":"done","actor_ref":"u-1","note":"直した"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	ch := h.ledger.lastChanges
	if ch.Status == nil || *ch.Status != "done" {
		t.Fatalf("状態が渡っていない: %+v", ch)
	}
	if ch.Terminal == nil || !*ch.Terminal {
		t.Error("終端かどうかが渡っていない")
	}
	if ch.ActorRef != "u-1" || ch.Note != "直した" {
		t.Errorf("変えた人と理由が渡っていない: %+v", ch)
	}
	if f := h.ledger.filterSnapshot(); len(f.Sources) != 1 || f.Sources[0] != "app_one" {
		t.Errorf("範囲が渡っていない: %+v", f)
	}
}

// 開いている状態へ戻すときは、終端ではないことが渡る（closed_at が空へ戻る）。
func TestPatchReopenClearsTerminal(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = true
	if w := h.patch(`{"status":"in_progress"}`); w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if ch := h.ledger.lastChanges; ch.Terminal == nil || *ch.Terminal {
		t.Errorf("終端でないことが渡っていない: %+v", ch.Terminal)
	}
}

// 期日は null で空へ戻せる。「触らない」と「空にする」を区別する。
func TestPatchClearsDueWithNull(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = true
	if w := h.patch(`{"promised_due":null}`); w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	ch := h.ledger.lastChanges
	if ch.PromisedDue == nil || *ch.PromisedDue != "" {
		t.Errorf("空にする指示が渡っていない: %+v", ch.PromisedDue)
	}
	if ch.HoldUntil != nil {
		t.Errorf("書いていない項目が渡っている: %+v", ch.HoldUntil)
	}
}

// 設定の一覧に無い状態・承認は受けない。
func TestPatchRejectsUnknownValues(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.patch(`{"status":"pending_review"}`), http.StatusBadRequest, errUnknownStatus)
	wantError(t, h.patch(`{"approval_state":"maybe"}`), http.StatusBadRequest, errUnknownApproval)
	if calls, _ := h.ledger.snapshot(); calls != 0 {
		t.Error("一覧に無い値で台帳が呼ばれた")
	}
}

// 状態と承認は空にできない（列が NOT NULL）。
func TestPatchRejectsNullStatus(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.patch(`{"status":null}`), http.StatusBadRequest, respond.InvalidRequest)
}

// 保留の解除期限は、承認が保留になるときだけ付けられる。
func TestPatchHoldNeedsHoldState(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.patch(`{"hold_until":"2026-10-01"}`), http.StatusBadRequest, errHoldWithoutHoldState)

	h.ledger.itemFound = true
	w := h.patch(`{"approval_state":"on_hold","hold_until":"2026-10-01"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("同じ要求で保留にするなら通るはず: %d (%s)", w.Code, w.Body.String())
	}
	// 解除は期限を空にするだけでできる（承認を動かさなくてよい）。
	if w := h.patch(`{"hold_until":null}`); w.Code != http.StatusOK {
		t.Fatalf("期限を空へ戻せない: %d (%s)", w.Code, w.Body.String())
	}
}

// 変えるものが 1 つも無い要求は受けない。何も起きないのに 200 が返る形を作らない。
func TestPatchRejectsEmptyChange(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.patch(`{"actor_ref":"u-1"}`), http.StatusBadRequest, respond.InvalidRequest)
	wantError(t, h.patch(`{"title":"別の題名"}`), http.StatusBadRequest, respond.InvalidRequest)
}

// 範囲の外の番号は「無い」と同じ。
func TestPatchHidesOutOfScope(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = false
	wantError(t, h.patch(`{"status":"done"}`), http.StatusNotFound, errNotFound)
}

// --- 人・返事・つながり --------------------------------------------------------

// 同じことに当たった人を 1 人足す。既にいた人なら 200。
func TestAddPerson(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = true
	h.ledger.personAdded = true

	w := h.send(http.MethodPost, "/v1/cases/app_one-41/people", keyAppOne, `{"reporter_ref":"u-2"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if h.ledger.lastPerson != "u-2" {
		t.Errorf("人が渡っていない: %q", h.ledger.lastPerson)
	}

	h.ledger.personAdded = false
	w = h.send(http.MethodPost, "/v1/cases/app_one-41/people", keyAppOne, `{"reporter_ref":"u-2"}`)
	if w.Code != http.StatusOK {
		t.Errorf("2 回目は 200 のはず: %d", w.Code)
	}
}

// 誰を足すのか書いていない要求は受けない。
func TestAddPersonNeedsRef(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.send(http.MethodPost, "/v1/cases/app_one-41/people", keyAppOne, `{}`),
		http.StatusBadRequest, respond.InvalidRequest)
}

// 返事を足す。**本文はログに出ない。**
func TestAddReply(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = true

	w := h.send(http.MethodPost, "/v1/cases/app_one-41/replies", keyAppOne,
		`{"body":"直りました。確認してください。","author_ref":"u-9"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if h.ledger.lastReply != [2]string{"直りました。確認してください。", "u-9"} {
		t.Errorf("返事が渡っていない: %+v", h.ledger.lastReply)
	}
	if strings.Contains(h.logs.String(), "直りました") {
		t.Errorf("返事の本文がログに出ている: %s", h.logs.String())
	}
}

// つながりは 3 種類だけ。2 回目は足さずに同じ id を返す。
func TestAddLink(t *testing.T) {
	h := newHarness(t)
	h.ledger.itemFound = true
	h.ledger.linkAdded = true

	w := h.send(http.MethodPost, "/v1/cases/app_one-41/links", keyAppOne,
		`{"type":"commit","ref":"0123456789abcdef"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}

	h.ledger.linkAdded = false
	w = h.send(http.MethodPost, "/v1/cases/app_one-41/links", keyAppOne,
		`{"type":"commit","ref":"0123456789abcdef"}`)
	if w.Code != http.StatusOK {
		t.Errorf("2 回目は 200 のはず: %d (%s)", w.Code, w.Body.String())
	}

	wantError(t, h.send(http.MethodPost, "/v1/cases/app_one-41/links", keyAppOne,
		`{"type":"ticket","ref":"x"}`), http.StatusBadRequest, respond.InvalidRequest)
}

// --- 添付 ---------------------------------------------------------------------

// 置き場が設定に無ければ、添付の口だけが動かない。
func TestAttachmentsOffWhenUnconfigured(t *testing.T) {
	h := newHarness(t)
	w := h.upload(keyAppOne, "/v1/cases/app_one-41/attachments", "a.png", "image/png", []byte("x"))
	wantError(t, w, http.StatusServiceUnavailable, errAttachmentsOff)
}

// 受け取ったバイト列から台帳が sha256 を計算する。送り主の計算値は受け取らない。
func TestAddAttachment(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.itemFound = true
	data := []byte("screenshot-bytes")

	w := h.upload(keyAppOne, "/v1/cases/app_one-41/attachments", "画面.png", "image/png", data)
	if w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])

	var got attachmentResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("応答が JSON として読めない: %v (%s)", err, w.Body.String())
	}
	if got.SHA256 != want {
		t.Errorf("ハッシュが違う: %q want %q", got.SHA256, want)
	}
	if got.Size != uint64(len(data)) || got.Filename != "画面.png" || got.Mime != "image/png" {
		t.Errorf("応答が違う: %+v", got)
	}
	if a := h.ledger.lastAttach; a.SHA256 != want || a.SizeBytes != uint64(len(data)) {
		t.Errorf("台帳へ渡した値が違う: %+v", a)
	}

	name, put, _ := h.files.snapshot()
	if !bytes.Equal(put, data) {
		t.Error("置き場へ渡したバイト列が違う")
	}
	if !strings.HasPrefix(name, "app_one-41-"+want[:16]+"-") {
		t.Errorf("置き場に付けた名前が違う: %q", name)
	}
}

// 1 件の上限を超えたものは、切らずに拒否する。
func TestAttachmentTooLarge(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.itemFound = true
	w := h.upload(keyAppOne, "/v1/cases/app_one-41/attachments", "big.bin", "", bytes.Repeat([]byte("x"), 1025))
	wantError(t, w, http.StatusRequestEntityTooLarge, errFileTooLarge)
	if name, _, _ := h.files.snapshot(); name != "" {
		t.Error("拒否したのに置き場へ置いている")
	}
}

// 置き場の上限に当たったら受け付けない。古いものを押し出さない。
func TestAttachmentStoreFull(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.itemFound = true
	h.ledger.used = 4000
	w := h.upload(keyAppOne, "/v1/cases/app_one-41/attachments", "a.bin", "", bytes.Repeat([]byte("x"), 200))
	wantError(t, w, http.StatusInsufficientStorage, errStoreFull)
	if name, _, _ := h.files.snapshot(); name != "" {
		t.Error("上限に当たったのに置き場へ置いている")
	}
}

// 行が入らなかったら、置いたものを引き取る。実体の無い参照を残さない。
func TestAttachmentRollsBackBlob(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.itemFound = false // 範囲の外
	w := h.upload(keyAppOne, "/v1/cases/app_two-41/attachments", "a.bin", "", []byte("x"))
	wantError(t, w, http.StatusNotFound, errNotFound)
	if _, _, removed := h.files.snapshot(); len(removed) != 1 {
		t.Errorf("置いたものを引き取っていない: %+v", removed)
	}
}

// 道の区切りを含む名前で、格納フォルダの外を指せない。
func TestAttachmentFilenameIsSafe(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.itemFound = true
	if w := h.upload(keyAppOne, "/v1/cases/app_one-41/attachments",
		"../../etc/passwd", "", []byte("x")); w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	name, _, _ := h.files.snapshot()
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		t.Errorf("置き場に付けた名前に道が残っている: %q", name)
	}
	if h.ledger.lastAttach.Filename != "passwd" {
		t.Errorf("台帳へ渡したファイル名が違う: %q", h.ledger.lastAttach.Filename)
	}
}

// 実体を消しても行は残る。理由が付く。
func TestRemoveAttachment(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.attachFound = true
	h.ledger.attachment = store.Attachment{ID: 11, Ref: "app_one-41-abc-a.png"}

	w := h.send(http.MethodDelete, "/v1/cases/app_one-41/attachments/11", keyAppOne, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if _, _, removed := h.files.snapshot(); len(removed) != 1 || removed[0] != "app_one-41-abc-a.png" {
		t.Errorf("置き場から消していない: %+v", removed)
	}
	if h.ledger.lastMark != [2]string{"11", store.DeletedByUser} {
		t.Errorf("印が違う: %+v", h.ledger.lastMark)
	}
}

// 置き場に無かったときも印は付ける。付けないと、次の実行でまた見に行く。
func TestRemoveAttachmentMissingAtStore(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.attachFound = true
	h.ledger.attachment = store.Attachment{ID: 11, Ref: "gone"}
	h.files.removeErr = blobs.ErrNotFound

	if w := h.send(http.MethodDelete, "/v1/cases/app_one-41/attachments/11", keyAppOne, ""); w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if h.ledger.lastMark != [2]string{"11", store.DeletedMissing} {
		t.Errorf("理由が違う: %+v", h.ledger.lastMark)
	}
}

// 既に消えている添付へもう一度送っても、同じ答えを返す。
func TestRemoveAttachmentTwice(t *testing.T) {
	h := newAttachHarness(t)
	h.ledger.attachFound = true
	h.ledger.attachment = store.Attachment{ID: 11, Deleted: true, Reason: store.DeletedExpired}

	w := h.send(http.MethodDelete, "/v1/cases/app_one-41/attachments/11", keyAppOne, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status が違う: %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), store.DeletedExpired) {
		t.Errorf("消えた理由を返していない: %s", w.Body.String())
	}
	if _, _, removed := h.files.snapshot(); len(removed) != 0 {
		t.Error("既に消えているのに置き場を触っている")
	}
}

// 番号が無い・階層が深すぎる形は弾く。
func TestCaseItemRejectsMalformedPath(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/v1/cases/",
		"/v1/cases/app_one-41/attachments/9/extra",
	} {
		wantError(t, h.get(keyAppOne, path), http.StatusNotFound, errNotFound)
	}
}

// --- 口の形 -------------------------------------------------------------------

// /v1/cases が受けるのは受付（POST）と一覧（GET）だけ。
// 一覧を書き換える口は無い（案件を変えるのは 1 件ずつ）。
func TestCasesRejectsOtherMethods(t *testing.T) {
	h := newHarness(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := httptest.NewRequest(m, "/v1/cases", nil)
		r.Header.Set("Authorization", "Bearer "+keyAppOne)
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		wantError(t, w, http.StatusMethodNotAllowed, respond.InvalidRequest)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("生きているかの口が 200 を返さない: %d", w.Code)
	}
}

// --- ログ ---------------------------------------------------------------------

// 受付 1 件につき 1 行。個人の情報を書かない。
func TestLogLineHasNoPersonalData(t *testing.T) {
	h := newHarness(t)
	if w := h.post(keyAppOne, humanCase("")); w.Code != http.StatusCreated {
		t.Fatalf("status が違う: %d", w.Code)
	}

	out := h.logs.String()
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 0 {
		t.Errorf("1 件で 1 行になっていない: %q", out)
	}
	for _, forbidden := range []string{
		reporter,       // 報告した人
		"月次の画面が保存できない", // 題名
		"保存を押すとページの先頭に戻る。", // 本文
		"app.example.com", // 発生場所の URL
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("ログに %q が出ている: %s", forbidden, out)
		}
	}
	for _, want := range []string{`app=app_one`, `source=app_one`, `kind=bug`, `number=app_one-234`} {
		if !strings.Contains(out, want) {
			t.Errorf("ログに %q が無い: %s", want, out)
		}
	}
}
