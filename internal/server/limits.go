package server

import (
	"fmt"
	"net/http"
	"time"

	"crest/internal/config"
	"crest/internal/logging"
	"crest/internal/vars"
)

func (i *Instance) wrapLimitReq(h http.Handler, use *config.LimitReqUse) (http.Handler, error) {
	l := i.reqLimiters[use.Zone]
	if l == nil {
		return nil, fmt.Errorf("unknown limit_req zone %q", use.Zone)
	}
	keyT := i.reqZoneKeys[use.Zone]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyT.Expand(vars.Request(r))
		ok, delay := l.Take(key, use.Burst)
		if !ok {
			i.errLog.Logf(logging.LevelWarn, "limiting requests, zone %q, client %q", use.Zone, key)
			http.Error(w, "503 Service Temporarily Unavailable", http.StatusServiceUnavailable)
			return
		}
		if !use.NoDelay && delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	}), nil
}

func (i *Instance) wrapLimitConn(h http.Handler, use *config.LimitConnUse) (http.Handler, error) {
	l := i.connLimiters[use.Zone]
	if l == nil {
		return nil, fmt.Errorf("unknown limit_conn zone %q", use.Zone)
	}
	keyT := i.connZoneKeys[use.Zone]
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := keyT.Expand(vars.Request(r))
		if !l.Acquire(key, use.Max) {
			i.errLog.Logf(logging.LevelWarn, "limiting connections, zone %q, client %q", use.Zone, key)
			http.Error(w, "503 Service Temporarily Unavailable", http.StatusServiceUnavailable)
			return
		}
		defer l.Release(key)
		h.ServeHTTP(w, r)
	}), nil
}
