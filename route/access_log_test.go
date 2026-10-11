package route

import (
	"strings"
	"testing"
)

func TestAccessLogFormatHasNoURI(t *testing.T) {
	if strings.Contains(accessLogFormat, "${uri}") {
		t.Fatalf("access log format logs the request URI, which carries the console token: %s", accessLogFormat)
	}

	if !strings.Contains(accessLogFormat, `"path":"${path}"`) {
		t.Fatalf("access log format does not log the path: %s", accessLogFormat)
	}
}
