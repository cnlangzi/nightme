package wf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/httpclient"
)

// maxBody is the largest workflow file the install accepts. A file
// beyond it is refused: the hub publishes workflows, not archives.
const maxBody = 1 << 20

// probeTimeout is how long a tier has to answer the reachability
// probe. A dead tier must not hold the install up.
const probeTimeout = 10 * time.Second

// downloadTimeout is how long a tier has to serve the file.
const downloadTimeout = httpclient.DefaultTimeout

// probe asks a tier whether it can serve the file.
//
// A HEAD request says it; a tier that refuses HEAD (405, 501) is
// asked with OPTIONS instead. A tier that cannot be reached, or that
// answers with a status of 400 or above, is not asked for the file:
// a dead tier otherwise costs a whole transfer timeout.
func probe(ctx context.Context, client *http.Client, src Source, addr Address) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	resp, err := do(ctx, client, http.MethodHead, src.URL(addr))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		// The tier does not do HEAD: ask it with OPTIONS.
		resp, err = do(ctx, client, http.MethodOptions, src.URL(addr))
		if err != nil {
			return err
		}
	}
	if resp.StatusCode >= 400 {
		return &tierStatus{tier: src.Name, status: resp.StatusCode}
	}
	return nil
}

// do makes one request and reads the response head. The body is
// closed: the probe never reads it.
func do(ctx context.Context, client *http.Client, method, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	return resp, nil
}

// fetch downloads the workflow file of addr from one tier.
//
// The body is read through a limit, so a tier cannot fill the disk,
// and the transfer is reported to onProgress as it goes. The total is
// the content length the tier announced, or 0 when it announced
// none.
func fetch(ctx context.Context, client *http.Client, src Source, addr Address,
	onProgress func(transferred, total int64)) ([]byte, error) {

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL(addr), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &tierStatus{tier: src.Name, status: resp.StatusCode}
	}

	total := resp.ContentLength
	if total < 0 {
		total = 0
	}
	var body bytes.Buffer
	buf := make([]byte, 32*1024)
	var read int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			read += int64(n)
			if read > maxBody {
				return nil, fmt.Errorf("the workflow file is larger than %d bytes", maxBody)
			}
			body.Write(buf[:n])
			if onProgress != nil {
				onProgress(read, total)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return body.Bytes(), nil
}

// tierStatus is a tier that answered with a status instead of the
// file.
//
// The status is what tells a workflow that is not published apart
// from a tier that is out of reach.
type tierStatus struct {
	tier   string
	status int
}

// Error implements the error interface.
func (e *tierStatus) Error() string {
	return fmt.Sprintf("%s answered %d %s", e.tier, e.status, http.StatusText(e.status))
}

// httpClient is the client the tiers are asked through.
func httpClient() *http.Client {
	return httpclient.Default()
}

// sha256hex returns the hex digest of the content.
func sha256hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// writeFile writes the content to path atomically.
//
// The content goes into a temporary file in the destination's own
// directory and is renamed over the target: a reader never sees a
// half-written workflow, and an interrupted write leaves nothing
// behind. The directory is created when it is missing.
func writeFile(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return replaceFile(name, path)
}
