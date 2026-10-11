package v2

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// SameOrigin reports whether the browser that opened the WebSocket was on the
// page it is connecting to: Origin's host equals Host. Without the check, a
// page on another site holding a leaked token could drive a VM's keyboard.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return strings.EqualFold(u.Host, r.Host)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	Subprotocols:    []string{"binary"},
	CheckOrigin:     SameOrigin,
}

// OpenConsole relays the VM's VNC stream between the browser (noVNC) and the
// unix socket QEMU listens on.
func (v *VirtManagement) OpenConsole(ctx echo.Context, id codegen.VMID, _ codegen.OpenConsoleParams) error {
	if !websocket.IsWebSocketUpgrade(ctx.Request()) {
		message := "not a WebSocket upgrade"
		return ctx.JSON(http.StatusBadRequest, codegen.BaseResponse{Message: &message})
	}

	if !SameOrigin(ctx.Request()) {
		message := "the console must be opened from the dashboard"
		return ctx.JSON(http.StatusForbidden, codegen.BaseResponse{Message: &message})
	}

	socketPath, err := v.vms.ConsoleSocket(id)
	if err != nil {
		return fail(ctx, err)
	}

	vnc, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fail(ctx, err)
	}
	defer vnc.Close()

	ws, err := upgrader.Upgrade(ctx.Response(), ctx.Request(), nil)
	if err != nil {
		// the upgrader has answered already
		return nil
	}
	defer ws.Close()

	logger.Info("console opened", zap.String("vm", id.String()))
	relay(ws, vnc)
	logger.Info("console closed", zap.String("vm", id.String()))

	return nil
}

func relay(ws *websocket.Conn, vnc net.Conn) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	// VNC -> browser
	go func() {
		defer stop()

		buf := make([]byte, 32<<10)
		for {
			n, err := vnc.Read(buf)
			if n > 0 {
				if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// browser -> VNC
	go func() {
		defer stop()

		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.BinaryMessage && kind != websocket.TextMessage {
				continue
			}
			if _, err := vnc.Write(data); err != nil {
				return
			}
		}
	}()

	<-done

	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
}
