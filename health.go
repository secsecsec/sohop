package sohop

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"
)

var healthClient = createHealthClient()

const certWarning = 72 * time.Hour

func createHealthClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}
	return client
}

type healthStatus struct {
	Response  string        `json:"response"`
	LatencyMS time.Duration `json:"latency_ms"`
}

type healthReport struct {
	sync.RWMutex
	response []byte
	allOk    bool
}

func (s Server) performCheck() {
	s.health.Lock()
	defer s.health.Unlock()

	allOk := true
	responses := make(map[string]healthStatus)

	var lock sync.Mutex // responses
	var wg sync.WaitGroup

	for k, v := range s.Config.Upstreams {
		k := k
		v := v

		wg.Add(1)
		go func() {
			defer wg.Done()

			healthCheck := v.HealthCheck
			if healthCheck == "" {
				healthCheck = v.URL
			}

			start := time.Now()
			resp, err := healthClient.Get(healthCheck)
			elapsed := time.Since(start) / time.Millisecond

			lock.Lock()
			defer lock.Unlock()
			if err == nil {
				responses[k] = healthStatus{Response: resp.Status, LatencyMS: elapsed}
				if resp.StatusCode != 200 {
					allOk = false
				}
			} else {
				responses[k] = healthStatus{Response: err.Error(), LatencyMS: elapsed}
				allOk = false
			}
		}()
	}

	certResponse := make(map[string]interface{}, 5)
	wg.Add(1)
	go func() {
		defer wg.Done()
		data, err := ioutil.ReadFile(s.Config.TLS.CertFile)
		if err != nil {
			certResponse["ok"] = false
			certResponse["error"] = err.Error()
			return
		}
		notBefore, notAfter, err := certValidity(data)
		if err != nil {
			certResponse["ok"] = false
			certResponse["error"] = err.Error()
			return
		}

		certResponse["expires_at"] = notAfter
		now := time.Now()
		if !notBefore.Before(now) {
			certResponse["error"] = "not yet valid"
			certResponse["valid_at"] = notBefore
			certResponse["ok"] = false
		} else if !notAfter.After(now) {
			certResponse["error"] = "expired"
			certResponse["ok"] = false
		} else if !notAfter.Add(-1 * certWarning).After(now) {
			certResponse["expires_in"] = notAfter.Sub(now).String()
			certResponse["error"] = "expires soon"
			certResponse["ok"] = false
		} else {
			certResponse["ok"] = true
		}
	}()

	wg.Wait()

	allOk = allOk && certResponse["ok"].(bool)

	res, err := json.MarshalIndent(struct {
		Upstreams map[string]healthStatus `json:"upstreams"`
		Cert      map[string]interface{}  `json:"cert"`
	}{
		Upstreams: responses,
		Cert:      certResponse,
	}, "", "  ")
	if err != nil {
		s.health.response = []byte("internal server error")
		s.health.allOk = false
		log.Print(err)
		return
	}

	s.health.allOk = allOk
	s.health.response = res
}

// HealthHandler checks each upstream and considers them healthy if they return
// a 200 response.  Also, the health check will fail if the TLS certificate will
// expire within 72 hours.
func (s Server) HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.health.RLock()
		defer s.health.RUnlock()

		w.Header().Add("Content-Type", "application/json; charset=UTF-8")

		if !s.health.allOk {
			w.WriteHeader(503)
		}
		w.Write(s.health.response)
	})
}
