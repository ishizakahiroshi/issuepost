// 添付の口。
//
//	POST   /v1/cases/{number}/attachments        添付を足す
//	DELETE /v1/cases/{number}/attachments/{id}   実体を消す。**行は残る**
//
// **添付は台帳へ送る**（設計文書 §3）。アプリが置き場へ直接置く形にしない。
// 台帳が受け取ったバイト列から sha256 を計算するので、保管したものが送られたものと
// 同じであることを台帳自身が言える。送り主が計算した値を信じる形だと、誰も検証できない。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ishizakahiroshi/doorpost/oplog"
	"github.com/ishizakahiroshi/doorpost/respond"

	"github.com/ishizakahiroshi/issuepost/internal/blobs"
	"github.com/ishizakahiroshi/issuepost/internal/store"
)

// 添付の口だけのエラーの語。
const (
	// errAttachmentsOff は置き場が設定されていない。
	// **案件そのものは受け付ける**（設計文書 §6）。添付が付かないだけ。
	errAttachmentsOff respond.Code = "attachments_not_configured"

	// errStoreFull は置き場の上限に当たった。
	// **古いものから押し出さない。**押し出すと、まだ必要なものが消える（§6）。
	errStoreFull respond.Code = "attachment_store_full"

	// errFileTooLarge は 1 件が大きすぎる。切らずに拒否する（題名と同じ扱い）。
	errFileTooLarge respond.Code = "file_too_large"
)

// 添付の部の名前。multipart の中でこの名前の部だけを見る。
const filePartName = "file"

// blobNameRunes は置き場に付ける名前の長さの上限。
// 置き場の側にも名前の長さの制限があるので、こちらで先に収める。
const blobNameRunes = 120

// Files は添付の実体の置き場。テストで差し替えられるように口だけを取る。
type Files interface {
	Put(ctx context.Context, name string, data []byte) (ref string, err error)
	Remove(ctx context.Context, ref string) error
}

// handleAttachments は /v1/cases/{number}/attachments... を振り分ける。
func (s *Server) handleAttachments(w http.ResponseWriter, r *http.Request, number, id string) {
	switch {
	case id == "" && r.Method == http.MethodPost:
		s.handleAddAttachment(w, r, number)
	case id != "" && r.Method == http.MethodDelete:
		s.handleRemoveAttachment(w, r, number, id)
	default:
		respond.Error(w, http.StatusMethodNotAllowed, respond.InvalidRequest)
	}
}

// attachmentResponse は足したときに返すもの。
//
// id は文字列で返す。数として返すと、桁が増えたときに読む側の言語によって
// 丸められることがある（JSON の数は倍精度で読まれる）。
type attachmentResponse struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	Mime     string `json:"mime"`
	Size     uint64 `json:"size_bytes"`
	SHA256   string `json:"sha256"`
}

// handleAddAttachment は添付を 1 件足す。
func (s *Server) handleAddAttachment(w http.ResponseWriter, r *http.Request, number string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}
	if s.files == nil || !s.cfg.Attachments.Configured() {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errAttachmentsOff))
		respond.Error(w, http.StatusServiceUnavailable, errAttachmentsOff)
		return
	}

	data, filename, mimeType, code := s.readFilePart(r)
	if code != "" {
		status := http.StatusBadRequest
		if code == errFileTooLarge {
			status = http.StatusRequestEntityTooLarge
		}
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", code))
		respond.Error(w, status, code)
		return
	}

	// 置き場の上限に当たっていないかを、置く前に見る。
	used, err := s.ledger.UsedBytes(r.Context())
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if used+uint64(len(data)) > uint64(s.cfg.Attachments.TotalLimitBytes) {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errStoreFull))
		respond.Error(w, http.StatusInsufficientStorage, errStoreFull)
		return
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	// **置いてから行を入れる。**先に行を入れると、置くのに失敗したときに
	// 実体の無い参照が残る。逆の順なら、失敗したぶんは置き場の孤児で済み、
	// こちらから消しに行ける。
	ref, err := s.files.Put(r.Context(), blobName(number, digest, filename), data)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errAttachmentsOff))
		respond.Error(w, http.StatusServiceUnavailable, errAttachmentsOff)
		return
	}

	id, found, err := s.ledger.AddAttachment(r.Context(), filterFor(scope), number, store.NewAttachment{
		Ref:       ref,
		Filename:  filename,
		Mime:      mimeType,
		SizeBytes: uint64(len(data)),
		SHA256:    digest,
	})
	if err != nil || !found {
		// 行が入らなかったので、置いたものを引き取る。
		// 消せなくても、記録に無い実体が 1 つ残るだけで、参照は生まれない。
		if rmErr := s.files.Remove(r.Context(), ref); rmErr != nil {
			s.logger.Print(
				oplog.Str("app", app.Name),
				oplog.Str("op", "attach"),
				oplog.Str("error", "orphan_blob"),
				oplog.Str("ref", ref),
			)
		}
		if err != nil {
			respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
			return
		}
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	// **ファイル名を書かない。**人が付けた名前で、運用ログに残す理由が無い。
	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "attach"),
		oplog.Str("number", number),
		oplog.Int("bytes", len(data)),
	)
	respond.JSON(w, http.StatusCreated, attachmentResponse{
		ID:       strconv.FormatUint(id, 10),
		Filename: filename,
		Mime:     mimeType,
		Size:     uint64(len(data)),
		SHA256:   digest,
	})
}

// readFilePart は multipart の中から 1 つぶんのファイルを読む。
//
// **全体をメモリに置く。**sha256 を自分で計算すると決めた以上、バイト列は
// どこかで全部通る。1 件の上限は設定で決まっていて、それを超えたところで切る。
func (s *Server) readFilePart(r *http.Request) (data []byte, filename, mimeType string, code respond.Code) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", "", respond.InvalidRequest
	}
	max := s.cfg.Attachments.MaxFileBytes
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, "", "", respond.InvalidRequest
		}
		if err != nil {
			return nil, "", "", respond.InvalidRequest
		}
		if part.FormName() != filePartName || part.FileName() == "" {
			_ = part.Close()
			continue
		}

		// 上限より 1 バイト多く読む。ちょうど上限で切ると、超えていることと
		// ちょうどであることが区別できない。
		buf, err := io.ReadAll(io.LimitReader(part, max+1))
		_ = part.Close()
		if err != nil {
			return nil, "", "", respond.InvalidRequest
		}
		if int64(len(buf)) > max {
			return nil, "", "", errFileTooLarge
		}
		if len(buf) == 0 {
			return nil, "", "", respond.InvalidRequest
		}

		name := safeFilename(part.FileName())
		if name == "" {
			return nil, "", "", respond.InvalidRequest
		}
		return buf, name, partMime(part.Header.Get("Content-Type")), ""
	}
}

// handleRemoveAttachment は実体を消す。**行は残す**（設計文書 §6）。
func (s *Server) handleRemoveAttachment(w http.ResponseWriter, r *http.Request, number, rawID string) {
	app, scope := s.authorize(w, r)
	if scope == nil {
		return
	}
	id, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil || id == 0 {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}

	a, found, err := s.ledger.FindAttachment(r.Context(), filterFor(scope), number, id)
	if err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}
	if !found {
		respond.Error(w, http.StatusNotFound, errNotFound)
		return
	}
	if a.Deleted {
		// 既に消えている。同じ要求を 2 回受けても同じ答えを返す。
		respond.JSON(w, http.StatusOK, map[string]any{"deleted": true, "reason": a.Reason})
		return
	}
	if s.files == nil || !s.cfg.Attachments.Configured() {
		respond.Error(w, http.StatusServiceUnavailable, errAttachmentsOff)
		return
	}

	// **失敗しても記録は進める**（設計文書 §6）。理由だけを分ける。
	// 置き場に無かったのなら missing_at_store。届かなかったのなら、
	// 人が消したという記録のまま残し、実体は定期の片づけが拾う。
	reason := store.DeletedByUser
	switch err := s.files.Remove(r.Context(), a.Ref); {
	case err == nil:
	case errors.Is(err, blobs.ErrNotFound):
		reason = store.DeletedMissing
	default:
		s.logger.Print(
			oplog.Str("app", app.Name),
			oplog.Str("op", "detach"),
			oplog.Str("error", "store_unreachable"),
			oplog.Str("ref", a.Ref),
		)
	}

	if err := s.ledger.MarkAttachmentDeleted(r.Context(), id, reason); err != nil {
		s.logger.Print(oplog.Str("app", app.Name), oplog.Str("error", errLedgerUnavailable))
		respond.Error(w, http.StatusServiceUnavailable, errLedgerUnavailable)
		return
	}

	s.logger.Print(
		oplog.Str("app", app.Name),
		oplog.Str("op", "detach"),
		oplog.Str("number", number),
		oplog.Str("reason", reason),
	)
	respond.JSON(w, http.StatusOK, map[string]any{"deleted": true, "reason": reason})
}

// partMime は部が名乗った種類を整える。名乗らないもの・読めないものは既定に倒す。
func partMime(v string) string {
	t, _, err := mime.ParseMediaType(v)
	if err != nil || t == "" {
		return "application/octet-stream"
	}
	if utf8.RuneCountInString(t) > maxMimeRunes {
		return "application/octet-stream"
	}
	return t
}

// safeFilename は置き場と台帳に置ける形へ整える。
//
// **道の区切りと制御文字を落とす。**送られてきた名前をそのまま置き場の
// 名前にすると、`../` を含む名前で格納フォルダの外を指せる。
func safeFilename(v string) string {
	v = strings.ReplaceAll(v, "\\", "/")
	v = filepath.Base(v)
	if v == "." || v == "/" || v == ".." {
		return ""
	}
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	v = strings.TrimSpace(v)
	if n := utf8.RuneCountInString(v); n > maxFileRunes {
		v = string([]rune(v)[:maxFileRunes])
	}
	return v
}

// blobName は置き場に付ける名前。
//
// 案件の番号と内容のハッシュを先頭に置く。置き場を人が見たときに、
// どの案件のものかが分かり、同じ名前のファイルが衝突しない。
func blobName(number, digest, filename string) string {
	stem := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			return r
		case r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, filename)
	name := number + "-" + digest[:16] + "-" + stem
	if n := utf8.RuneCountInString(name); n > blobNameRunes {
		name = string([]rune(name)[:blobNameRunes])
	}
	return name
}
