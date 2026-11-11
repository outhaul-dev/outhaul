// Command dnsadapter answers lego's httpreq DNS provider and forwards each
// change to pebble-challtestsrv, so integration_ingress.sh exercises
// OUTHAUL_ACME_DNS_ENV with lego's named-variable path.
//
//	dnsadapter -listen :8080 -challtestsrv http://host:8055
//
// POST /present {"fqdn":"...","value":"..."} -> challtestsrv POST /set-txt {"host":"...","value":"..."}
// POST /cleanup {"fqdn":"..."}               -> challtestsrv POST /clear-txt {"host":"..."}
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type legoMessage struct {
	FQDN  string `json:"fqdn"`
	Value string `json:"value"`
}

type challMessage struct {
	Host  string `json:"host"`
	Value string `json:"value,omitempty"`
}

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	chall := flag.String("challtestsrv", "", "pebble-challtestsrv base URL, e.g. http://127.0.0.1:8055")
	flag.Parse()
	if *chall == "" {
		fmt.Fprintln(flag.CommandLine.Output(), "dnsadapter: -challtestsrv is required")
		flag.Usage()
		os.Exit(2)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	post := func(path string, msg challMessage) error {
		body, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		resp, err := client.Post(strings.TrimRight(*chall, "/")+path, "application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("%s: %s: %s", path, resp.Status, data)
		}
		log.Printf("%s %s", path, msg.Host)
		return nil
	}
	handle := func(path string, fn func(legoMessage) error) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var msg legoMessage
			if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := fn(msg); err != nil {
				log.Printf("%s: %v", path, err)
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}
	http.HandleFunc("/present", handle("/present", func(m legoMessage) error {
		return post("/set-txt", challMessage{Host: m.FQDN, Value: m.Value})
	}))
	http.HandleFunc("/cleanup", handle("/cleanup", func(m legoMessage) error {
		return post("/clear-txt", challMessage{Host: m.FQDN})
	}))
	log.Printf("dnsadapter on %s, forwarding to %s", *listen, *chall)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
