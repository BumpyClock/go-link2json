package link2json

import (
	"context"
	urlpkg "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/cenkalti/backoff/v4"
	"github.com/chromedp/chromedp"
	"github.com/gocolly/colly"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

var (
	// cch is the in-memory cache for storing metadata responses.
	cch             = cache.New(24*time.Hour, 48*time.Hour) // Example: 24-hour expiration
	userAgent       string
	LINK2JSON_DEBUG bool

	// Concurrency control
	maxConcurrentBrowsers = 5
	chromedpSemaphore     = make(chan struct{}, maxConcurrentBrowsers)
)

// init initializes the module, setting up logging and user agent.
func init() {
	// Set default debug mode to true
	LINK2JSON_DEBUG = true

	// Override debug mode based on environment variable
	if value, exists := os.LookupEnv("LINK2JSON_DEBUG"); exists {
		if parsedValue, err := strconv.ParseBool(value); err == nil {
			LINK2JSON_DEBUG = parsedValue
		}
	}

	// Configure logging based on debug mode
	if LINK2JSON_DEBUG {
		logrus.Info("[Link2Json] Debug mode is enabled. To disable set env LINK2JSON_DEBUG=false.")
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}

	// Set default User-Agent
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) " +
		"Chrome/129.0.0.0 Safari/537.36"

	// Override User-Agent if provided via environment variable
	if value, exists := os.LookupEnv("LINK2JSON_USER_AGENT"); exists {
		userAgent = value
	}
}

// GetMetadataWithRetry fetches and returns metadata for the given URL with retries.
func GetMetadataWithRetry(targetURL string) (*MetaDataResponseItem, error) {
	var result *MetaDataResponseItem
	operation := func() error {
		var err error
		result, err = GetMetadata(targetURL)
		return err
	}

	err := backoff.Retry(operation, backoff.NewExponentialBackOff())
	if err != nil {
		return nil, err
	}

	return result, nil
}

// GetMetadata fetches and returns metadata for the given URL.
// It uses Colly for static pages and Chromedp for dynamically rendered pages.
func GetMetadata(targetURL string) (*MetaDataResponseItem, error) {
	// Check cache first
	if cached, found := cch.Get(targetURL); found {
		logrus.Debugf("[GetMetadata] Cache hit for URL: %s", targetURL)
		return cached.(*MetaDataResponseItem), nil
	}

	// Initialize result
	result := &MetaDataResponseItem{
		URL:    targetURL,
		Images: []WebImage{},
	}
	result.Domain = getBaseDomain(targetURL)

	// Determine if the URL requires JavaScript rendering
	useChromedp := requiresJavaScript(targetURL)

	if useChromedp {
		logrus.Debugf("[GetMetadata] Using Chromedp for URL: %s", targetURL)
		htmlContent, err := fetchRenderedHTML(targetURL)
		if err != nil {
			logrus.Errorf("[GetMetadata] Chromedp failed for URL %s: %v", targetURL, err)
			return nil, err
		}

		// Parse the rendered HTML using goquery
		if err := parseHTMLWithGoquery(htmlContent, result); err != nil {
			logrus.Errorf("[GetMetadata] Failed to parse HTML with goquery for URL %s: %v", targetURL, err)
			return nil, err
		}
	} else {
		logrus.Debugf("[GetMetadata] Using Colly for URL: %s", targetURL)
		// Use Colly for static pages
		if err := scrapeWithColly(targetURL, result); err != nil {
			logrus.Errorf("[GetMetadata] Colly failed for URL %s: %v", targetURL, err)
			return nil, err
		}
	}

	// Cache the result
	cch.Set(targetURL, result, cache.DefaultExpiration)

	return result, nil
}

// fetchRenderedHTML uses Chromedp to navigate to the URL and return the rendered HTML.
func fetchRenderedHTML(targetURL string) (string, error) {
	// Acquire semaphore for concurrency control
	chromedpSemaphore <- struct{}{}
	defer func() { <-chromedpSemaphore }()

	// Define Chromedp allocator options
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),                        // Ensure headless mode
		chromedp.Flag("disable-gpu", true),                     // Disable GPU usage
		chromedp.Flag("no-sandbox", true),                      // Disable sandboxing
		chromedp.Flag("disable-dev-shm-usage", true),           // Overcome limited resource problems
		chromedp.Flag("blink-settings", "imagesEnabled=false"), // Optional: Disable image loading for performance
		chromedp.UserAgent(userAgent),                          // Use the configured User-Agent
		// chromedp.ExecPath("/usr/bin/chromium-browser"), // Uncomment and set if necessary
	)

	// Create a new allocator context
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	// Create a new Chromedp context from the allocator context
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	// Set a timeout to prevent hanging
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var htmlContent string
	err := chromedp.Run(ctx,
		chromedp.Navigate(targetURL),
		chromedp.WaitVisible("body", chromedp.ByQuery), // Wait for the body to be visible
		chromedp.OuterHTML("html", &htmlContent, chromedp.ByQuery),
	)
	if err != nil {
		logrus.Error("[fetchRenderedHTML] Chromedp navigation failed:", err)
		return "", err
	}

	return htmlContent, nil
}

// parseHTMLWithGoquery parses the HTML content using goquery and fills the result.
func parseHTMLWithGoquery(htmlContent string, result *MetaDataResponseItem) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return err
	}

	// Extract <title>
	doc.Find("title").Each(func(i int, s *goquery.Selection) {
		if result.Title == "" {
			result.Title = s.Text()
		}
	})

	// Extract meta description
	doc.Find(`meta[name="description"]`).Each(func(i int, s *goquery.Selection) {
		if desc, exists := s.Attr("content"); exists && result.Description == "" {
			result.Description = desc
		}
	})

	// Extract favicon
	doc.Find(`link[rel="icon"], link[rel="shortcut icon"], ` +
		`link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`).Each(func(i int, s *goquery.Selection) {
		if result.Favicon == "" {
			if href, exists := s.Attr("href"); exists {
				parsedHref, err := urlpkg.Parse(href)
				if err != nil || !parsedHref.IsAbs() {
					result.Favicon = result.Domain + href
				} else {
					result.Favicon = href
				}
			}
		}
	})

	// Extract OpenGraph site name
	doc.Find(`meta[property="og:site_name"]`).Each(func(i int, s *goquery.Selection) {
		if sitename, exists := s.Attr("content"); exists && result.Sitename == "" {
			result.Sitename = sitename
		}
	})

	// Extract OpenGraph images
	doc.Find(`meta[property="og:image"]`).Each(func(i int, s *goquery.Selection) {
		img := WebImage{}
		if imgURL, exists := s.Attr("content"); exists {
			img.URL = imgURL
		}
		// Extract additional og:image properties
		parent := s.Parent()
		if parent != nil {
			// og:image:alt
			if alt, exists := parent.Find(`meta[property="og:image:alt"]`).Attr("content"); exists {
				img.Alt = alt
			}
			// og:image:type
			if imgType, exists := parent.Find(`meta[property="og:image:type"]`).Attr("content"); exists {
				img.Type = imgType
			}
			// og:image:width
			if widthStr, exists := parent.Find(`meta[property="og:image:width"]`).Attr("content"); exists {
				if width, err := strconv.Atoi(widthStr); err == nil {
					img.Width = width
				}
			}
			// og:image:height
			if heightStr, exists := parent.Find(`meta[property="og:image:height"]`).Attr("content"); exists {
				if height, err := strconv.Atoi(heightStr); err == nil {
					img.Height = height
				}
			}
		}
		result.Images = append(result.Images, img)
	})

	// Fallback to og:title if sitename is still empty
	if result.Sitename == "" {
		doc.Find(`meta[property="og:title"]`).Each(func(i int, s *goquery.Selection) {
			if sitename, exists := s.Attr("content"); exists && result.Sitename == "" {
				result.Sitename = sitename
			}
		})
	}

	return nil
}

// scrapeWithColly uses the Colly library to scrape metadata from static pages.
func scrapeWithColly(targetURL string, result *MetaDataResponseItem) error {
	c := colly.NewCollector(
		colly.UserAgent(userAgent),
	)

	webImage := WebImage{}

	// Set up Colly callbacks
	c.OnRequest(func(r *colly.Request) {
		logrus.Debugf("[Colly] Visiting %s", r.URL.String())
		r.Headers.Set("User-Agent", userAgent)
	})

	c.OnHTML("title", func(e *colly.HTMLElement) {
		if result.Title == "" {
			result.Title = e.Text
		}
	})

	c.OnHTML(`meta[name="description"]`, func(e *colly.HTMLElement) {
		if result.Description == "" {
			result.Description = e.Attr("content")
		}
	})

	c.OnHTML(`link[rel="icon"], link[rel="shortcut icon"], `+
		`link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`, func(e *colly.HTMLElement) {
		if result.Favicon == "" {
			href := e.Attr("href")
			logrus.Debugf("[Colly] Favicon found: %s", href)
			parsedHref, err := urlpkg.Parse(href)
			if err != nil || !parsedHref.IsAbs() {
				result.Favicon = result.Domain + href
			} else {
				result.Favicon = href
			}
		}
	})

	c.OnHTML(`meta[property="og:site_name"]`, func(e *colly.HTMLElement) {
		if result.Sitename == "" {
			result.Sitename = e.Attr("content")
		}
	})

	c.OnHTML(`meta[property="og:image"]`, func(e *colly.HTMLElement) {
		webImage.URL = e.Attr("content")
	})

	c.OnHTML(`meta[property="og:image:alt"]`, func(e *colly.HTMLElement) {
		webImage.Alt = e.Attr("content")
	})

	c.OnHTML(`meta[property="og:image:type"]`, func(e *colly.HTMLElement) {
		webImage.Type = e.Attr("content")
	})

	c.OnHTML(`meta[property="og:image:width"]`, func(e *colly.HTMLElement) {
		width, err := strconv.Atoi(e.Attr("content"))
		if err == nil {
			webImage.Width = width
		}
	})

	c.OnHTML(`meta[property="og:image:height"]`, func(e *colly.HTMLElement) {
		height, err := strconv.Atoi(e.Attr("content"))
		if err == nil {
			webImage.Height = height
		}
	})

	c.OnScraped(func(r *colly.Response) {
		result.Images = append(result.Images, webImage)
		logrus.Debugf("[Colly] Scraping finished for %s", r.Request.URL.String())
	})

	// Visit the target URL
	if err := c.Visit(targetURL); err != nil {
		return err
	}

	// If sitename is still empty, attempt to scrape the base domain
	if result.Sitename == "" {
		baseDomain := getBaseDomain(targetURL)
		if baseDomain != "" {
			logrus.Debugf("[Colly] Attempting to scrape base domain: %s", baseDomain)
			c2 := colly.NewCollector(
				colly.UserAgent(userAgent),
			)

			c2.OnHTML(`meta[property="og:title"]`, func(e *colly.HTMLElement) {
				if result.Sitename == "" {
					result.Sitename = e.Attr("content")
				}
			})

			if err := c2.Visit(baseDomain); err != nil {
				logrus.Errorf("[Colly] Failed to visit base domain %s: %v", baseDomain, err)
			}
		}
	}

	return nil
}

// requiresJavaScript determines if the given URL likely requires JavaScript rendering.
// This function can be enhanced with more sophisticated logic or configuration.
func requiresJavaScript(targetURL string) bool {
	parsedURL, err := urlpkg.Parse(targetURL)
	if err != nil {
		logrus.Warnf("[requiresJavaScript] Failed to parse URL %s: %v", targetURL, err)
		return false
	}

	hostname := parsedURL.Hostname()

	// List of domains known to require JavaScript
	jsRequiredDomains := map[string]bool{
		"youtube.com":       true,
		"www.youtube.com":   true,
		"facebook.com":      true,
		"www.facebook.com":  true,
		"twitter.com":       true,
		"www.twitter.com":   true,
		"instagram.com":     true,
		"www.instagram.com": true,
		// Add more domains as needed
	}

	if _, exists := jsRequiredDomains[hostname]; exists {
		return true
	}

	// Additionally, check for common JavaScript frameworks or patterns
	// This is a simplistic check and can be improved
	jsIndicators := []string{"/js/", "/javascript/", "/dynamic/"}
	for _, indicator := range jsIndicators {
		if strings.Contains(parsedURL.Path, indicator) {
			return true
		}
	}

	return false
}

// getBaseDomain extracts the base domain (scheme + host) from the URL.
func getBaseDomain(targetURL string) string {
	parsedURL, err := urlpkg.Parse(targetURL)
	if err != nil {
		logrus.Warnf("[getBaseDomain] Failed to parse URL %s: %v", targetURL, err)
		return ""
	}

	return parsedURL.Scheme + "://" + parsedURL.Host
}

// ParsePage is a placeholder for future functionality.
// Currently, it does nothing.
func ParsePage(url string) {
	// Future implementation can go here
}
