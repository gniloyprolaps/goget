package main

import (
	"flag"
	"fmt"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	AppName    = "goget"
	AppVersion = "0.0.1a"
	defaultChunkSize = 1024 * 1024
	maxRetries       = 3
	maxRedirects     = 15
)

var ChromeVersion = calculateChromeVersion(time.Now())

var defaultHeaders = []string{
	fmt.Sprintf("User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", ChromeVersion),
	"Accept: */*",
	"Accept-Encoding: identity",
	"Connection: keep-alive",
	"Cache-Control: no-cache",
}

const (
	ParamUrl           = "URL для загрузки файла (https://...)"
	ParamThreads       = "количество потоков на загрузку (максимальных соединений на хост будет вдвое больше)"
	ParamDir           = "путь куда будет сохраняет скачанный файл (/home/user/downloads)"
	ParamHeaders       = "заголовки в формате curl (можно указать несколько раз: -H 'Key: Value')"
	ParamVersion       = "показать версию программы"
	InfVersion         = "%s-%s\n"
	ErrFatal           = "ОШИБКА при загрузке файла"
	ErrNoUrl           = "URL является обязательным! (goget -url https://...)"
	ErrCreateOutputDir = "не удалось создать указанную папку для сохранения, нет прав? создай вручную."
	ErrFileSize        = "не удалось определить размер файла"
	ErrFileName        = "не удалось определить имя файла"
	ErrOverRedirects   = "слишком много перенаправлений"
	ErrStatCode        = "unexpected status code: %d"
	ErrIncompleteWrite = "incomplete write: got %d bytes, expected %d"
	WarnNoAutoFileName = "ПРЕДУПРЕЖДЕНИЕ: автоматически получить имя файла не удалось"
	WarnNoAutoFileSize = "ПРЕДУПРЕЖДЕНИЕ: не удалось определить размер файла:"
	InfNameFound       = "имя файла найдено"
	InfProgress        = "скачивание"
	InfOptChunk        = "оптимальный размер частей для файла %s: %s"
	InfDlComplete      = "загрузка завершена"
)

type DownloadConfig struct {
	URL       string
	OutputDir string
	Threads   int
	ChunkSize int64
	Headers   []string
}

type DownloadPart struct {
	start int64
	end   int64
}

var startTime = time.Now()

func createOptimizedClient(threads int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     30 * time.Second,
			DisableCompression:  true,
			MaxConnsPerHost:     threads * 2,
		},
		Timeout: 5 * time.Minute,
	}
}

func calculateOptimalChunkSize(fileSize int64) int64 {
	if fileSize < 10*1024*1024 {
		return 256 * 1024
	}

	if fileSize < 100*1024*1024 {
		return 1 * 1024 * 1024
	}

	if fileSize < 1024*1024*1024 {
		return 4 * 1024 * 1024
	}

	if fileSize < 10*1024*1024*1024 {
		return 8 * 1024 * 1024
	}

	return 16 * 1024 * 1024
}

func humanReadableSize(bytes int64) string {
	const (
		B  = 1
		KB = 1024 * B
		MB = 1024 * KB
		GB = 1024 * MB
		TB = 1024 * GB
		PB = 1024 * TB
	)

	var size float64
	var unit string

	switch {
	case bytes >= PB:
		size = float64(bytes) / PB
		unit = "PB"
	case bytes >= TB:
		size = float64(bytes) / TB
		unit = "TB"
	case bytes >= GB:
		size = float64(bytes) / GB
		unit = "GB"
	case bytes >= MB:
		size = float64(bytes) / MB
		unit = "MB"
	case bytes >= KB:
		size = float64(bytes) / KB
		unit = "KB"
	default:
		size = float64(bytes)
		unit = "B"
	}

	return fmt.Sprintf("%.2f %s", size, unit)
}

func DownloadFile(config DownloadConfig) error {
	if err := os.MkdirAll(config.OutputDir, 0755); err != nil {
		return fmt.Errorf("%s: %v", ErrCreateOutputDir, err)
	}

	fileName, err := getFileNameWithFastRedirects(config.URL, config.Threads)
	if err != nil {
		log.Printf("%s: %v", WarnNoAutoFileName, err)
		fileName = path.Base(config.URL)
	}

	filePath := path.Join(config.OutputDir, fileName)
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	client := createOptimizedClient(config.Threads)

	totalSize, err := determineFileSize(client, config.URL, config.Headers)
	if err != nil {
		log.Printf("%s: %v", WarnNoAutoFileSize, err)
		return downloadUnknownSize(config, file)
	}

	chunkSize := config.ChunkSize
	if chunkSize == defaultChunkSize {
		chunkSize = calculateOptimalChunkSize(totalSize)
		log.Printf(InfOptChunk, humanReadableSize(totalSize), humanReadableSize(chunkSize))
	}

	var downloadedBytes int64
	var wg sync.WaitGroup

	partsChan := make(chan DownloadPart, config.Threads*2)
	errChan := make(chan error, config.Threads)

	progressTicker := time.NewTicker(50 * time.Millisecond)
	defer progressTicker.Stop()

	go func() {
		var lastBytes int64
		var lastTime = time.Now()

		for range progressTicker.C {
			current := atomic.LoadInt64(&downloadedBytes)
			percent := float64(current) / float64(totalSize) * 100
			elapsed := time.Since(lastTime).Seconds()
			if elapsed > 0 {
				speed := float64(current-lastBytes) / elapsed
				fmt.Printf("\r%s: %.2f%% (%s/%s, %s/s)", InfProgress, percent, humanReadableSize(current), humanReadableSize(totalSize), humanReadableSize(int64(speed)))
			}

			lastBytes = current
			lastTime = time.Now()
		}
	}()

	for i := 0; i < config.Threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			localClient := createOptimizedClient(config.Threads)

			for part := range partsChan {
				var lastErr error
				for attempt := 0; attempt < maxRetries; attempt++ {
					lastErr = downloadPartOptimized(localClient, config.URL, part, file, &downloadedBytes, config.Headers)
					if lastErr == nil {
						break
					}
					time.Sleep(time.Second * time.Duration(attempt+1))
				}

				if lastErr != nil {
					errChan <- lastErr
					return
				}
			}
		}()
	}

	for i := int64(0); i < totalSize; i += chunkSize {
		end := i + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		partsChan <- DownloadPart{start: i, end: end}
	}
	close(partsChan)

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	elapsedTime := time.Since(startTime).Seconds()
	averageSpeed := float64(totalSize) / elapsedTime
	fmt.Printf("\r%s: 100.00%% (%s, %s/s)\n", InfDlComplete, humanReadableSize(totalSize), humanReadableSize(int64(averageSpeed)))
	return nil
}

func addHeaders(req *http.Request, headers []string) {
	for _, header := range headers {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) == 2 {
			req.Header.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}
}

func downloadPartOptimized(client *http.Client, url string, part DownloadPart, file *os.File, downloadedBytes *int64, headers []string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", part.start, part.end))
	addHeaders(req, headers)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf(ErrStatCode, resp.StatusCode)
	}

	written, err := io.CopyN(&fastFileWriter{
		file:            file,
		offset:          part.start,
		downloadedBytes: downloadedBytes,
	}, resp.Body, part.end-part.start+1)
	if err != nil {
		return err
	}
	if written != part.end-part.start+1 {
		return fmt.Errorf(ErrIncompleteWrite, written, part.end-part.start+1)
	}
	return nil
}

type fastFileWriter struct {
	file            *os.File
	offset          int64
	downloadedBytes *int64
}

func (w *fastFileWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(p, w.offset)
	if err != nil {
		return n, err
	}

	w.offset += int64(n)
	atomic.AddInt64(w.downloadedBytes, int64(n))
	return n, nil
}

func downloadUnknownSize(config DownloadConfig, file *os.File) error {
	client := createOptimizedClient(config.Threads)
	resp, err := client.Get(config.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	downloaded, err := io.Copy(file, resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("\r%s: %s\n", InfDlComplete, humanReadableSize(downloaded))
	return nil
}

func extractFilenameFromContentDisposition(contentDisposition string) string {
	if contentDisposition == "" {
		return ""
	}

	parts := strings.Split(contentDisposition, ";")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "filename=") || strings.HasPrefix(part, "filename*=") {
			filename := strings.Trim(strings.TrimPrefix(part, "filename="), "\"")
			filename = strings.Trim(strings.TrimPrefix(filename, "filename*=UTF-8''"), "\"")
			return filename
		}
	}
	return ""
}

func getFileNameWithFastRedirects(urlStr string, threads int) (string, error) {
	client := createOptimizedClient(threads)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if filename := extractFilenameFromContentDisposition(req.Response.Header.Get("Content-Disposition")); filename != "" {
			return fmt.Errorf("%s: %s", InfNameFound, filename)
		}

		if len(via) > maxRedirects {
			return fmt.Errorf(ErrOverRedirects)
		}
		return nil
	}

	resp, err := client.Head(urlStr)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if filename := extractFilenameFromContentDisposition(resp.Header.Get("Content-Disposition")); filename != "" {
		return filename, nil
	}

	parsedURL, err := url.Parse(resp.Request.URL.String())
	if err == nil {
		filename := path.Base(parsedURL.Path)
		if idx := strings.Index(filename, "?"); idx != -1 {
			filename = filename[:idx]
		}
		return filename, nil
	}

	return "", fmt.Errorf(ErrFileName)
}

func determineFileSize(client *http.Client, url string, headers []string) (int64, error) {
	var lastSize int64

	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Response != nil {
			if req.Response.ContentLength > 0 {
				lastSize = req.Response.ContentLength
			}
			if len(via) > maxRedirects {
				return fmt.Errorf(ErrOverRedirects)
			}
		}
		return nil
	}

	resp, err := client.Head(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.ContentLength > 0 {
		return resp.ContentLength, nil
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	addHeaders(req, headers)

	resp, err = client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	contentRange := resp.Header.Get("Content-Range")
	if contentRange != "" {
		parts := strings.Split(contentRange, "/")
		if len(parts) == 2 {
			size, err := strconv.ParseInt(parts[1], 10, 64)
			if err == nil && size > 0 {
				return size, nil
			}
		}
	}

	if lastSize > 0 {
		return lastSize, nil
	}

	return 0, fmt.Errorf(ErrFileSize)
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return strings.Join(*i, ", ")
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func mergeHeaders(userHeaders []string) []string {
	headerMap := make(map[string]string)
	caser := cases.Title(language.English)

	for _, header := range defaultHeaders {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) == 2 {
			key := strings.ToLower(strings.TrimSpace(parts[0]))
			headerMap[key] = strings.TrimSpace(parts[1])
		}
	}

	for _, header := range userHeaders {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) == 2 {
			key := strings.ToLower(strings.TrimSpace(parts[0]))
			headerMap[key] = strings.TrimSpace(parts[1])
		}
	}

	var result []string
	for key, value := range headerMap {
		words := strings.Split(key, "-")
		for i := range words {
			words[i] = caser.String(words[i])
		}
		canonicalKey := strings.Join(words, "-")
		result = append(result, fmt.Sprintf("%s: %s", canonicalKey, value))
	}

	return result
}

func calculateChromeVersion(currentTime time.Time) string {
	// https://chromiumdash.appspot.com/releases?platform=Android
	baseVersion := struct {
		major int
		minor int
		build int
		patch int
		date  time.Time
	}{
		major: 133,
		minor: 0,
		build: 6943,
		patch: 39,
		date:  time.Date(2024, 1, 30, 0, 0, 0, 0, time.UTC),
	}

	if currentTime.Before(baseVersion.date) {
		return fmt.Sprintf("%d.%d.%d.%d", baseVersion.major, baseVersion.minor, baseVersion.build, baseVersion.patch)
	}

	daysSince := int(currentTime.Sub(baseVersion.date).Hours() / 24)

	majorBumps := daysSince / 60
	buildBumps := daysSince / 45
	patchBumps := daysSince / 14

	if currentTime.Month() == 12 || currentTime.Month() == 1 {
		patchBumps = daysSince / 21
	}

	major := baseVersion.major
	if majorBumps > 0 {
		major += 1
	}

	build := baseVersion.build + buildBumps
	patch := baseVersion.patch + patchBumps

	return fmt.Sprintf("%d.%d.%d.%d", major, baseVersion.minor, build, patch)
}

func main() {
	showVersion := flag.Bool("v", false, ParamVersion)
	link := flag.String("url", "", ParamUrl)
	outputDir := flag.String("P", ".", ParamDir)
	maxThreads := flag.Int("t", 8, ParamThreads)
	var headers arrayFlags
	flag.Var(&headers, "H", ParamHeaders)
	flag.Parse()

	if *showVersion {
		fmt.Printf(InfVersion, AppName, AppVersion)
		fmt.Printf("Chrome version in User-Agent: " + ChromeVersion + "\n")
		os.Exit(0)
	}

	if *link == "" {
		flag.Usage()
		log.Fatal(ErrNoUrl)
	}

	config := DownloadConfig{
		URL:       *link,
		OutputDir: *outputDir,
		Threads:   *maxThreads,
		ChunkSize: defaultChunkSize,
		Headers:   mergeHeaders(headers),
	}

	err := DownloadFile(config)
	if err != nil {
		log.Fatalf("%s: %v", ErrFatal, err)
	}
}
