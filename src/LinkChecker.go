package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/PuerkitoBio/purell"
	"golang.org/x/net/html"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

var (
	root          = flag.String("root", "htTp://www.http.net", "Root to crawl")
	verbose       = flag.Bool("verbose", true, "verbose")
	debug         = flag.Bool("debug", false, "debug")
	externalLinks = flag.Bool("externalLinks", true, "Check external links")
)

var wg sync.WaitGroup        // outstanding fetches
var urlq = make(chan string) // URLs to crawl

type urlFrag struct {
	url, frag string
}

var (
	mu          sync.Mutex
	crawled     = make(map[string]bool)      // URL without fragment -> true
	neededFrags = make(map[urlFrag][]string) // URL#frag -> who needs it
)

// Owned by crawlLoop goroutine:
var (
	linkSources = make(map[string][]string) // url no fragment -> sources
	fragExists  = make(map[urlFrag]bool)
	problems    []string
)

func parseHtml(httpBody io.Reader) (links []string, ids []string) {
	linkSeen := map[string]bool{}
	page := html.NewTokenizer(httpBody)

	for {
		tokenType := page.Next()
		if tokenType == html.ErrorToken {
			return links, ids
		}

		token := page.Token()
		if tokenType == html.StartTagToken {
			for _, attr := range token.Attr {
				if attr.Key == "id" {
					ids = append(links, attr.Val)
				}
			}
			if token.DataAtom.String() == "a" {
				for _, attr := range token.Attr {
					if attr.Key == "href" {
						href := attr.Val
						if !linkSeen[href] {
							linkSeen[href] = true
							links = append(links, href)
						}
					}
				}
			}
		}

	}
}

// url may contain a #fragment, and the fragment is then noted as needing to exist.
func crawl(url string, sourceURL string) {
	mu.Lock()
	defer mu.Unlock()
	var frag string
	if i := strings.Index(url, "#"); i >= 0 {
		frag = url[i+1:]
		url = url[:i]
		if frag != "" {
			uf := urlFrag{url, frag}
			neededFrags[uf] = append(neededFrags[uf], sourceURL)
		}
	}
	if crawled[url] {
		return
	}
	crawled[url] = true

	wg.Add(1)
	go func() {
		urlq <- url
	}()
}

func addProblem(url, errmsg string) {
	msg := fmt.Sprintf("Error on %s: %s (from %s)", url, errmsg, linkSources[url])
	if *verbose {
		log.Print(msg)
	}
	problems = append(problems, msg)
}

func crawlLoop() {
	for url := range urlq {
		if err := doCrawl(url); err != nil {
			addProblem(url, err.Error())
		}
	}
}

func isSpecialProtocol(ref string) bool {
	return strings.HasPrefix(ref, "javascript:") || strings.HasPrefix(ref, "mailto:") || strings.HasPrefix(ref, "tel:")
}

func isAbsoluteUrl(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

func doCrawl(url string) error {
	defer wg.Done()
	if *verbose {
		log.Printf("  Crawling %s", url)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return err
	}
	// Handle redirects.
	if res.StatusCode/100 == 3 {
		newURL, err := res.Location()
		if err != nil {
			return fmt.Errorf("resolving redirect: %v", err)
		}
		if !strings.HasPrefix(newURL.String(), *root) {
			// Skip off-site redirects.
			return nil
		}
		crawl(newURL.String(), url)
		return nil
	}
	if res.StatusCode != 200 {
		return errors.New(res.Status)
	}
	// External links are only be checked for existance, so no further processing is needed
	if !strings.HasPrefix(url, *root) {
		return nil
	}

	contentType := res.Header.Get("Content-Type")
	if contentType == "" {
		return errors.New("No Content-Type set")
	}
	if !strings.HasPrefix(contentType, "text/html") {
		return nil
	}

	links, ids := parseHtml(res.Body)
	res.Body.Close()

	for _, ref := range links {
		if *debug {
			log.Printf("  links to %s", ref)
		}
		if isSpecialProtocol(ref) {
			continue
		}
		var dest string
		if isAbsoluteUrl(ref) {
			dest = ref
		} else {
			dest = *root + ref
		}

		if !*externalLinks && !strings.HasPrefix(dest, *root) {
			continue
		}

		normalizedDest, _ := purell.NormalizeURLString(dest, purell.FlagsSafe)

		linkSources[normalizedDest] = append(linkSources[normalizedDest], url)
		crawl(normalizedDest, url)
	}
	for _, id := range ids {
		if *debug {
			log.Printf(" url %s has #%s", url, id)
		}
		fragExists[urlFrag{url, id}] = true
	}
	return nil
}

func main() {
	flag.Parse()

	go crawlLoop()
	*root, _ = purell.NormalizeURLString(*root, purell.FlagsSafe)
	crawl(*root, "")

	wg.Wait()
	close(urlq)
	for uf, needers := range neededFrags {
		if !fragExists[uf] {
			problems = append(problems, fmt.Sprintf("Missing fragment for %+v from %v", uf, needers))
		}
	}

	for _, s := range problems {
		fmt.Println(s)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
}
