package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

type downloader struct {
	client           *http.Client
	maxDepth         int
	concurrencyLimit int
	visited          map[string]bool
	visitedMu        sync.Mutex
	host             string
	rootDir          string
	wg               sync.WaitGroup
	sem              chan struct{} // семафор для ограничения одновременных загрузок
	startURL         string
}

func new(limit int, timeout time.Duration) *downloader {
	return &downloader{
		client: &http.Client{
			Timeout: timeout,
		},
		concurrencyLimit: limit,
		visited:          make(map[string]bool),
		sem:              make(chan struct{}, limit),
	}
}

func main() {
	var startURL string
	var conLimit int
	var maxDepth int
	var timeout int

	flag.StringVar(&startURL, "u", "", "URL to mirror")
	flag.IntVar(&maxDepth, "d", 1, "Recursion depth")
	flag.IntVar(&conLimit, "n", 5, "Maximum number of concurrent downloads")
	flag.IntVar(&timeout, "t", 30, "HTTP client timeout in seconds")
	flag.Parse()

	if startURL == "" {
		log.Fatal("URL is required")
	}

	d := new(conLimit, time.Duration(timeout)*time.Second)
	d.startURL = startURL
	d.maxDepth = maxDepth

	start := time.Now()

	// берем стартовый урл
	// например github.com/andreyxaxa
	base, err := url.Parse(d.startURL)
	if err != nil {
		log.Fatal(err)
	}
	if base.Scheme == "" {
		base.Scheme = "https"
	}

	// запоминаем стартовый хост
	// например github.com
	d.host = base.Host
	d.rootDir = base.Host

	// создаем основную директорию
	if err := os.MkdirAll(d.rootDir, 0755); err != nil {
		log.Fatal(err)
	}

	// запускаем, глубина изначально 0
	d.download(base, 0)

	// ждем завершения всех горутин
	d.wg.Wait()

	fmt.Println("Done in:", time.Since(start))
}

func (d *downloader) download(u *url.URL, depth int) {
	// каждая загрузка выполняется в отдельной горутине
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()

		if depth > d.maxDepth {
			return
		}

		urlStr := u.String()

		d.visitedMu.Lock()
		if d.visited[urlStr] {
			d.visitedMu.Unlock()
			return
		}
		d.visited[urlStr] = true
		d.visitedMu.Unlock()

		// ограничение параллелизма
		d.sem <- struct{}{}
		defer func() { <-d.sem }()

		// делаем get-запрос на урл
		resp, err := d.client.Get(urlStr)
		log.Println(urlStr)
		fmt.Printf("HTTP request sent, awaiting response... %s\n", resp.Status)
		if err != nil {
			fmt.Printf("ERROR %s\n\n", resp.Status)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("ERROR %s\n\n", resp.Status)
			return
		}

		// создаем правильный путь для файла
		localPath := d.makeLocalPath(u)
		// создаем директорию
		if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
			fmt.Printf("Failed to create dir for %s: %v\n", localPath, err)
			return
		}
		fmt.Printf("Saving to: '%s'\n", filepath.Dir(localPath))

		contentType := resp.Header.Get("Content-Type")
		isHTML := strings.Contains(contentType, "text/html")

		// если не html - сразу сохраняем (io.Copy в файл)
		if !isHTML {
			d.saveNonHTML(localPath, resp.Body)
			fmt.Printf("'%s' saved\n\n", urlStr)
			return
		}

		// если html - нужно парсить
		// парсим html
		doc, err := html.Parse(resp.Body)
		if err != nil {
			fmt.Printf("Failed to parse HTML %s: %v\n\n", urlStr, err)
			return
		}

		// собираем абсолютные ссылки на ресурсы (css/js/img) и внутренние страницы
		// для дальнеййшего обхода
		resources, pages := d.collectLinks(doc, u)

		// рекурсивно обрабатываем урлы ресурсов, не увеличивая глубину
		// ресурс не считается переходом на новую страницу
		for _, res := range resources {
			d.download(res, depth)
		}

		// перезаписываем ссылки в распаршенном html на локальные
		d.rewriteLinks(doc, u, localPath)

		// сохраняем html (html.Render в файл)
		if err := d.saveHTML(localPath, doc); err != nil {
			fmt.Printf("Failed to save HTML %s: %v\n\n", localPath, err)
		}
		fmt.Printf("'%s' saved\n\n", urlStr)

		// рекурсивно обрабатываем урлы страниц, увеличивая глубину
		for _, page := range pages {
			d.download(page, depth+1)
		}
	}()
}

// например github.com/andreyxaxa/order_svc -> github.com + andreyxaxa/order_svc + index.html
// host + path + index.html (если в конце директория | или файл оканчивается на "/")
func (d *downloader) makeLocalPath(u *url.URL) string {
	path := filepath.Join(d.rootDir, filepath.Clean(u.Path))
	if u.Path == "" || strings.HasSuffix(u.Path, "/") || filepath.Ext(path) == "" {
		path = filepath.Join(path, "index.html")
	}
	return path
}

// os.Create() + io.Copy()
func (d *downloader) saveNonHTML(localPath string, body io.Reader) {
	f, err := os.Create(localPath)
	if err != nil {
		log.Printf("Failed to create file %s: %v", localPath, err)
		return
	}
	defer f.Close()
	io.Copy(f, body)
}

// os.Create() + html.Render()
func (d *downloader) saveHTML(localPath string, doc *html.Node) error {
	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return html.Render(f, doc)
}

// обход DOM-дерева
func traverse(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		traverse(c, fn)
	}
}

func (d *downloader) collectLinks(doc *html.Node, base *url.URL) (resources, pages []*url.URL) {
	traverse(doc, func(n *html.Node) {
		// тег?
		if n.Type != html.ElementNode {
			return
		}

		var attrKey string
		isPageLink := false

		// смотрим имя тега
		// просто будем разделять ресурсы и страницы
		switch n.Data {
		case "a":
			attrKey = "href"
			isPageLink = true
		case "link":
			attrKey = "href"
		case "script", "img":
			attrKey = "src"
		default:
			return
		}

		for i := range n.Attr {
			if n.Attr[i].Key != attrKey {
				continue
			}

			// берем абсолютный урл
			absURL, err := base.Parse(n.Attr[i].Val)
			if err != nil {
				continue
			}
			if absURL.Scheme == "" {
				absURL.Scheme = base.Scheme
			}
			// совпадает ли хост
			if absURL.Host != d.host || (absURL.Scheme != "http" && absURL.Scheme != "https") {
				continue
			}
			absURL.Fragment = ""

			// делим на страницы и ресурсы
			if isPageLink {
				pages = append(pages, absURL)
			} else {
				resources = append(resources, absURL)
			}
			break
		}
	})

	return resources, pages
}

func (d *downloader) rewriteLinks(doc *html.Node, base *url.URL, currentLocalPath string) {
	traverse(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}

		var attrKey string
		switch n.Data {
		case "a", "link":
			attrKey = "href"
		case "script", "img":
			attrKey = "src"
		default:
			return
		}

		for i := range n.Attr {
			if n.Attr[i].Key != attrKey {
				continue
			}

			// берем абсолютную ссылку
			absURL, err := base.Parse(n.Attr[i].Val)
			// совпадает ли хост
			if err != nil || absURL.Host != d.host {
				continue
			}

			// создаем локальное представление для абсолютного урла
			targetLocalPath := d.makeLocalPath(absURL)
			// создаем относительный путь от текущего(от которого запускалась 'rewriteLinks()' до локального представления абс. урла)
			relPath, err := filepath.Rel(filepath.Dir(currentLocalPath), targetLocalPath)
			if err != nil {
				continue
			}

			// перезаписываем глобальные пути на свои
			n.Attr[i].Val = relPath
			break
		}
	})
}
