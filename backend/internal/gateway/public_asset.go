package gateway

import (
	"errors"
	"io"
	"mime"
	"net/http"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (h *Handler) downloadPublicAsset(w http.ResponseWriter, r *http.Request) {
	// An anonymous public endpoint. Cookies, MCP credentials and author headers do
	// not grant anything: the exact reviewed Content context must permit access.
	if r.URL.RawQuery != "" {
		problem(w, 400, "invalid_request")
		return
	}
	if h.options.Asset == nil {
		problem(w, 503, "asset_unavailable")
		return
	}
	stream, err := h.options.Asset.DownloadPublicAsset(r.Context(), &pb.DownloadPublicAssetRequest{Reference: &pb.PublicAssetReference{TaskId: r.PathValue("taskId"), EntryId: r.PathValue("entryId"), AssetId: r.PathValue("assetId")}})
	if err != nil {
		assetError(w, err)
		return
	}
	h.serveAssetStream(w, r, stream.Recv)
}
func (h *Handler) serveAssetStream(w http.ResponseWriter, r *http.Request, recv func() (*pb.DownloadPublicAssetResponse, error)) {
	first, err := recv()
	if err != nil {
		assetError(w, err)
		return
	}
	meta := first.GetMetadata()
	if meta == nil || meta.GetBytes() < 1 {
		problem(w, 503, "asset_unavailable")
		return
	}
	// Never send success/download headers until the stream has authorized and
	// opened the actual object. No ETag/304, redirect or reusable storage grant.
	w.Header().Set("Content-Type", meta.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.Filename}))
	var offset int64
	fail := func(err error) {
		if offset == 0 {
			w.Header().Del("Content-Disposition")
			assetError(w, err)
			return
		}
		panic(http.ErrAbortHandler)
	}
	for {
		frame, e := recv()
		if errors.Is(e, io.EOF) {
			if offset != meta.Bytes {
				fail(status.Error(codes.Unavailable, "asset_unavailable"))
				return
			}
			return
		}
		if e != nil {
			fail(e)
			return
		}
		if frame.GetMetadata() != nil || frame.GetOffset() != offset || len(frame.GetData()) == 0 || offset+int64(len(frame.GetData())) > meta.Bytes {
			// Headers/bytes may already be on the wire: do not append Problem JSON to
			// a successful file or silently present a truncated stream as complete.
			fail(status.Error(codes.Unavailable, "asset_unavailable"))
			return
		}
		if _, err = w.Write(frame.GetData()); err != nil {
			return
		}
		offset += int64(len(frame.GetData()))
	}
}
