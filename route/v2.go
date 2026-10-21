package route

import (
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ReCasaOS/CasaOS-VirtManagement/codegen"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/config"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/service"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	v2Route "github.com/ReCasaOS/CasaOS-VirtManagement/route/v2"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

var (
	_swagger *openapi3.T

	V2APIPath string
	V2DocPath string

	// tokenInQueryRoutes are the routes, as echo matched them, that accept the
	// user's token as ?token=: websockets, because a browser cannot set a
	// header on one. No other route does. Matching c.Path() rather than the
	// request path keeps an encoded path from passing for one of these.
	tokenInQueryRoutes []string
)

func init() {
	swagger, err := codegen.GetSwagger()
	if err != nil {
		panic(err)
	}

	_swagger = swagger

	u, err := url.Parse(_swagger.Servers[0].URL)
	if err != nil {
		panic(err)
	}

	V2APIPath = strings.TrimRight(u.Path, "/")
	V2DocPath = "/doc" + V2APIPath

	tokenInQueryRoutes = []string{
		V2APIPath + "/vms/:id/console",
	}
}

func acceptsTokenInQuery(c echo.Context) bool {
	return c.Request().Method == http.MethodGet && slices.Contains(tokenInQueryRoutes, c.Path())
}

// isStatsRulesPut: the handler bounds and checks this body itself, so that any
// fault is the contract's bad_rules and an oversized body is never read whole.
func isStatsRulesPut(c echo.Context) bool {
	return c.Request().Method == http.MethodPut && c.Path() == V2APIPath+"/stats/:id/alerts"
}

// isImageUpload: the body is an image streamed to disk; the validator would
// read it whole. The handler checks the request itself.
func isImageUpload(c echo.Context) bool {
	return c.Request().Method == http.MethodPut && c.Path() == V2APIPath+"/images/upload"
}

func InitV2Router(conn *libvirt.Conn, vms *service.VMs, st *service.Stats) http.Handler {
	virtManagement := v2Route.NewVirtManagement(conn, vms, st)

	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.PATCH, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	})))

	// not on the console: a WebSocket is hijacked, not a response to compress
	e.Use(echo_middleware.GzipWithConfig(echo_middleware.GzipConfig{Skipper: acceptsTokenInQuery}))

	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{Format: accessLogFormat}))

	e.Use(echojwt.WithConfig(echojwt.Config{
		// loopback AND the internal secret: loopback alone is not an identity
		Skipper: func(c echo.Context) bool {
			return external.IsInternalRequest(c.RealIP(), c.Request().Header.Get(echo.HeaderAuthorization), config.CommonInfo.RuntimePath)
		},
		ParseTokenFunc: func(c echo.Context, token string) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				if token := c.Request().Header.Get(echo.HeaderAuthorization); token != "" || !acceptsTokenInQuery(c) {
					return []string{token}, nil
				}

				return []string{c.QueryParam("token")}, nil
			},
		},
	}))

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		Skipper: func(c echo.Context) bool { return isStatsRulesPut(c) || isImageUpload(c) },
	}))

	codegen.RegisterHandlersWithBaseURL(e, virtManagement, V2APIPath)

	return e
}

func InitV2DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V2DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V2DocPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
