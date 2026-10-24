package webui

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string   { return e.msg }
func (e *codedError) HTTPStatus() int { return e.code }

func TestWritePluginCallErrorKeepsClientStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"unknown plugin", ErrPluginNotFound, http.StatusNotFound},
		{"not running", ErrPluginNotRunning, http.StatusConflict},
		{"plugin rejects a bad value", &codedError{400, "token is empty"}, http.StatusBadRequest},
		{"plugin says not implemented", &codedError{501, "no device identity"}, http.StatusNotImplemented},
		{"plugin failed upstream", &codedError{502, "upstream 500"}, http.StatusBadGateway},
		{"plain failure", errors.New("dial tcp: refused"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePluginCallError(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
