package azhttp_test

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-again/az"
	"github.com/go-again/az/azhttp"
)

// page is a body big enough to be worth compressing (see azhttp.DefaultMinSize).
var page = []byte(strings.Repeat("<p>the quick brown fox jumps over the lazy dog</p>\n", 200))

// Wrap any handler to compress its responses for clients that accept zstd.
func Example() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(page)
	})

	srv := httptest.NewServer(azhttp.Handler(mux))
	defer srv.Close()

	// A plain client that asks for zstd gets a zstd body back.
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err := srv.Client().Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	frame, _ := io.ReadAll(resp.Body)
	plain, _ := az.Decompress(frame)

	fmt.Println("Content-Encoding:", resp.Header.Get("Content-Encoding"))
	fmt.Println("Vary:", resp.Header.Get("Vary"))
	fmt.Printf("%d bytes on the wire, %d after decoding\n", len(frame), len(plain))
	// Output:
	// Content-Encoding: zstd
	// Vary: Accept-Encoding
	// 75 bytes on the wire, 10200 after decoding
}

// Build the middleware once with options and reuse it across handlers.
func ExampleNewWrapper() {
	compress, err := azhttp.NewWrapper(
		azhttp.MinSize(2048),        // don't bother below 2 KiB
		azhttp.ZstdLevel(az.Level4), // trade CPU for ratio
		azhttp.EnableLZ4(false),     // public site: standard codings only
		azhttp.SuffixETag("-az"),    // keep encoded and identity ETags apart
		azhttp.ExceptContentTypes([]string{"text/event-stream"}),
	)
	if err != nil {
		log.Fatal(err)
	}

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Etag", `"v1"`)
		fmt.Fprintf(w, `{"items":%q}`, strings.Repeat("x", 4096))
	})

	srv := httptest.NewServer(compress(api))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "zstd")
	resp, err := srv.Client().Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	fmt.Println("Content-Encoding:", resp.Header.Get("Content-Encoding"))
	fmt.Println("Etag:", resp.Header.Get("Etag"))
	// Output:
	// Content-Encoding: zstd
	// Etag: "v1-az-zstd"
}

// Wrap a client transport to request and decode az codings transparently.
func ExampleTransport() {
	srv := httptest.NewServer(azhttp.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(page)
	})))
	defer srv.Close()

	client := &http.Client{
		Transport: azhttp.Transport(srv.Client().Transport),
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	// The body reads as plain bytes; the coding is already gone.
	plain, _ := io.ReadAll(resp.Body)
	fmt.Println("decoded on the fly:", resp.Uncompressed)
	fmt.Println("bytes:", len(plain))
	// Output:
	// decoded on the fly: true
	// bytes: 10200
}

// Inside a private network, prefer lz4 to spend bandwidth instead of CPU.
// Nothing changes for public clients: lz4 is only ever sent to a peer that named
// it in Accept-Encoding.
func ExampleTransportPreferLZ4() {
	srv := httptest.NewServer(azhttp.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(page)
	})))
	defer srv.Close()

	client := &http.Client{
		Transport: azhttp.Transport(srv.Client().Transport, azhttp.TransportPreferLZ4(true)),
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	plain, _ := io.ReadAll(resp.Body)
	fmt.Println("bytes:", len(plain))
	// Output:
	// bytes: 10200
}

// Accept compressed uploads, capping how far they may expand.
func ExampleDecompressRequests() {
	const maxBody = 1 << 20 // 1 MiB of decompressed body

	upload := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		fmt.Fprintf(w, "received %d bytes", len(received))
	})

	srv := httptest.NewServer(azhttp.DecompressRequests(maxBody)(upload))
	defer srv.Close()

	frame, err := az.Compress(page, az.Level3)
	if err != nil {
		log.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(frame))
	req.Header.Set("Content-Encoding", "zstd")

	resp, err := srv.Client().Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	answer, _ := io.ReadAll(resp.Body)
	fmt.Printf("sent %d compressed bytes; server %s\n", len(frame), answer)
	// Output:
	// sent 75 compressed bytes; server received 10200 bytes
}
