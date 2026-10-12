package resource

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/metacubex/mihomo/common/utils"
	mihomoHttp "github.com/metacubex/mihomo/component/http"
	"github.com/metacubex/mihomo/component/profile/cachefile"
	P "github.com/metacubex/mihomo/constant/provider"

	"github.com/metacubex/http"
)

const (
	DefaultHttpTimeout = time.Second * 20

	dirMode os.FileMode = 0o755
)

var (
	etag atomic.Bool
)

func ETag() bool {
	return etag.Load()
}

func SetETag(b bool) {
	etag.Store(b)
}

func safeWrite(path string, buf []byte) error {
	// Keep existing symlink destinations working, but publish regular resource
	// files by rename so an interrupted write cannot leave a truncated database
	// or provider cache behind.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		path = resolved
	}
	dir := filepath.Dir(path)

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return err
		}
	}

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("resource destination is not a regular file")
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".mihomo-resource-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	if _, err = f.Write(buf); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

type FileVehicle struct {
	path string
}

func (f *FileVehicle) Type() P.VehicleType {
	return P.File
}

func (f *FileVehicle) Path() string {
	return f.path
}

func (f *FileVehicle) Url() string {
	return "file://" + f.path
}

func (f *FileVehicle) Read(ctx context.Context, oldHash utils.HashType) (buf []byte, hash utils.HashType, err error) {
	buf, err = os.ReadFile(f.path)
	if err != nil {
		return
	}
	hash = utils.MakeHash(buf)
	return
}

func (f *FileVehicle) Proxy() string {
	return ""
}

func (f *FileVehicle) Write(buf []byte) error {
	return safeWrite(f.path, buf)
}

func NewFileVehicle(path string) *FileVehicle {
	return &FileVehicle{path: path}
}

type HTTPVehicle struct {
	url       string
	path      string
	proxy     string
	header    http.Header
	timeout   time.Duration
	sizeLimit int64
	inRead    func(response *http.Response)
	provider  P.ProxyProvider
}

func (h *HTTPVehicle) Url() string {
	return h.url
}

func (h *HTTPVehicle) Type() P.VehicleType {
	return P.HTTP
}

func (h *HTTPVehicle) Path() string {
	return h.path
}

func (h *HTTPVehicle) Proxy() string {
	return h.proxy
}

func (h *HTTPVehicle) Write(buf []byte) error {
	return safeWrite(h.path, buf)
}

func (h *HTTPVehicle) SetInRead(fn func(response *http.Response)) {
	h.inRead = fn
}

func (h *HTTPVehicle) Read(ctx context.Context, oldHash utils.HashType) (buf []byte, hash utils.HashType, err error) {
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	header := h.header
	useETag := ETag()
	setIfNoneMatch := false
	if useETag && oldHash.IsValid() {
		etagWithHash := cachefile.Cache().GetETagWithHash(h.url)
		if oldHash.Equal(etagWithHash.Hash) && etagWithHash.ETag != "" {
			if header == nil {
				header = http.Header{}
			} else {
				header = header.Clone()
			}
			header.Set("If-None-Match", etagWithHash.ETag)
			setIfNoneMatch = true
		}
	}
	resp, err := mihomoHttp.HttpRequest(ctx, h.url, http.MethodGet, header, nil, mihomoHttp.WithSpecialProxy(h.proxy))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if h.inRead != nil {
		h.inRead(resp)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if setIfNoneMatch && resp.StatusCode == http.StatusNotModified {
			return nil, oldHash, nil
		}
		err = errors.New(resp.Status)
		return
	}
	var reader io.Reader = resp.Body
	if h.sizeLimit > 0 {
		limit := h.sizeLimit
		if limit < math.MaxInt64 {
			limit++
		}
		reader = io.LimitReader(reader, limit)
	}
	buf, err = io.ReadAll(reader)
	if err != nil {
		return
	}
	if h.sizeLimit > 0 && int64(len(buf)) > h.sizeLimit {
		return nil, utils.HashType{}, errors.New("resource exceeds configured size limit")
	}
	hash = utils.MakeHash(buf)
	if useETag {
		cachefile.Cache().SetETagWithHash(h.url, cachefile.EtagWithHash{
			Hash: hash,
			ETag: resp.Header.Get("ETag"),
			Time: time.Now(),
		})
	}
	return
}

func NewHTTPVehicle(url string, path string, proxy string, header http.Header, timeout time.Duration, sizeLimit int64) *HTTPVehicle {
	return &HTTPVehicle{
		url:       url,
		path:      path,
		proxy:     proxy,
		header:    header,
		timeout:   timeout,
		sizeLimit: sizeLimit,
	}
}
