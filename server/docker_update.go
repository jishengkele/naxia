package server

// The host owns Docker and the deployment files. The application can only ask
// its Unix-socket worker to install a release tag, never supply commands/paths.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

type dockerUpdateStatus struct {
	Ready   bool   `json:"ready"`
	Running bool   `json:"running"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
}

func dockerUpdateCall(ctx context.Context, method, path string, body any) (dockerUpdateStatus, error) {
	var st dockerUpdateStatus
	socket := os.Getenv("ARTEX_DOCKER_UPDATE_SOCKET")
	if socket == "" {
		return st, fmt.Errorf("未配置宿主机 Docker 更新服务，请按 README 安装后再更新")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return st, err
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, method, "http://updater"+path, bytes.NewReader(b))
	if err != nil {
		return st, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return st, fmt.Errorf("宿主机 Docker 更新服务不可用")
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st); err != nil {
		return st, fmt.Errorf("宿主机更新服务响应无效")
	}
	if resp.StatusCode >= 400 {
		return st, fmt.Errorf("%s", st.Error)
	}
	return st, nil
}

func (s *Server) dockerUpdateStatus(w http.ResponseWriter, r *http.Request) {
	st, err := dockerUpdateCall(r.Context(), "GET", "/status", nil)
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, st)
}
