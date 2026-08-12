// Command http is a runnable demo of the azhttp middleware: a server that
// compresses its responses and a client that decodes them.
//
//	go run ./examples/http           # probe the server, print a summary, exit
//	go run ./examples/http -serve    # keep serving on -addr until interrupted
//
// With -serve you can poke at it by hand:
//
//	curl -sv --compressed-zstd http://localhost:8080/api | head
//	curl -s -H 'Accept-Encoding: zstd' http://localhost:8080/api | zstd -d | head
//	curl -s http://localhost:8080/api | head   # no Accept-Encoding: plain body
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/go-again/az"
	"github.com/go-again/az/azhttp"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "address to serve on with -serve")
	serve := flag.Bool("serve", false, "keep serving instead of running the probe")
	flag.Parse()

	// One wrapper, built at start-up and shared by every handler. Level 3 is
	// az's default zstd level; lz4 stays enabled for peers that ask for it by
	// name (a browser never will — "lz4" is not a registered content coding).
	compress, err := azhttp.NewWrapper(
		azhttp.MinSize(512),
		azhttp.ZstdLevel(az.Level3),
		azhttp.LZ4Level(az.Level1),
	)
	if err != nil {
		log.Fatalf("azhttp: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api", apiHandler)
	mux.HandleFunc("/stream", streamHandler)
	// Uploads may arrive compressed too; cap how far a body may expand.
	mux.Handle("/upload", azhttp.DecompressRequests(8<<20)(http.HandlerFunc(uploadHandler)))

	handler := compress(mux)

	if *serve {
		log.Printf("serving on http://%s (/api, /stream, /upload)", *addr)
		log.Fatal(http.ListenAndServe(*addr, handler))
	}

	probe(handler)
}

// apiHandler returns a chunk of very compressible JSON.
func apiHandler(w http.ResponseWriter, r *http.Request) {
	type item struct {
		ID    int    `json:"id"`
		Name  string `json:"name"`
		Notes string `json:"notes"`
	}
	items := make([]item, 200)
	for i := range items {
		items[i] = item{ID: i, Name: fmt.Sprintf("item-%03d", i), Notes: "the quick brown fox jumps over the lazy dog"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

// streamHandler writes a chunk a second, flushing each one. Compression follows
// the flushes, so a client sees each line as it is produced.
func streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	rc := http.NewResponseController(w)
	for i := range 5 {
		if _, err := fmt.Fprintf(w, "tick %d: %s\n", i, strings.Repeat("x", 200)); err != nil {
			return // client went away
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
}

// uploadHandler reads a body that DecompressRequests has already decoded.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	_, _ = fmt.Fprintf(w, "decoded %d bytes\n", n)
}

// probe runs the handler in a throwaway server and reports what each kind of
// client gets, so the effect of the middleware is visible without a browser.
func probe(handler http.Handler) {
	srv := httptest.NewServer(handler)
	defer srv.Close()

	fmt.Printf("%-28s %-18s %10s %10s\n", "client", "Content-Encoding", "wire", "decoded")
	fmt.Println(strings.Repeat("─", 70))

	for _, tc := range []struct {
		label      string
		acceptEnc  string // sent verbatim; empty means no header at all
		clientDecs bool   // let azhttp.Transport handle it
	}{
		{label: "no Accept-Encoding"},
		{label: "Accept-Encoding: gzip", acceptEnc: "gzip"},
		{label: "Accept-Encoding: zstd", acceptEnc: "zstd"},
		{label: "Accept-Encoding: lz4", acceptEnc: "lz4"},
		{label: "azhttp.Transport", clientDecs: true},
	} {
		client := srv.Client()
		if tc.clientDecs {
			client = &http.Client{Transport: azhttp.Transport(srv.Client().Transport)}
		}
		req, err := http.NewRequest(http.MethodGet, srv.URL+"/api", nil)
		if err != nil {
			log.Fatal(err)
		}
		if tc.acceptEnc != "" {
			req.Header.Set("Accept-Encoding", tc.acceptEnc)
		} else if !tc.clientDecs {
			// Stop net/http from adding its own gzip request.
			req.Header.Set("Accept-Encoding", "identity")
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		wire, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatal(err)
		}

		coding := resp.Header.Get("Content-Encoding")
		decoded := len(wire)
		wireSize := fmt.Sprint(len(wire))
		switch coding {
		case azhttp.EncodingZstd, azhttp.EncodingLZ4:
			plain, err := az.Decompress(wire)
			if err != nil {
				log.Fatalf("decompress %s: %v", coding, err)
			}
			decoded = len(plain)
		case "":
			if tc.clientDecs {
				// The transport stripped the coding and decoded the body, so
				// the compressed size is no longer observable from here.
				coding, wireSize = "(decoded by client)", "-"
			} else {
				coding = "-"
			}
		}
		fmt.Printf("%-28s %-18s %10s %10d\n", tc.label, coding, wireSize, decoded)
	}

	fmt.Println("\nRun with -serve to try it with curl or a browser.")
}
