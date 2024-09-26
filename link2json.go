package link2json

import (
	"context"
	urlpkg "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
	"github.com/gocolly/colly"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

var (
	// cch is the in-memory cache for storing metadata responses.
	cch = cache.New(24*time.Hour, 48*time.Hour) // Example: 24-hour expiration

	// chromedpWhitelist stores domains that require Chromedp for scraping.
	chromedpWhitelist = cache.New(cache.NoExpiration, cache.NoExpiration)

	// Mutex to protect concurrent access to chromedpWhitelist.
	whitelistMutex sync.RWMutex

	// userAgent is the User-Agent string used in HTTP requests.
	userAgent string

	// LINK2JSON_DEBUG enables or disables debug logging.
	LINK2JSON_DEBUG bool

	// chromedpSemaphore controls the maximum number of concurrent Chromedp instances.
	chromedpSemaphore = make(chan struct{}, 5) // Example: limit to 5 concurrent Chromedp instances
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

// GetMetadata fetches and returns metadata for the given URL.
// It first attempts to use Colly for scraping. If Colly fails to extract sufficient metadata,
// it falls back to Chromedp. Domains requiring Chromedp are cached for future requests.
func GetMetadata(targetURL string) (*MetaDataResponseItem, error) {
	// Normalize the URL
	normalizedURL, err := normalizeURL(targetURL)
	if err != nil {
		logrus.Errorf("[GetMetadata] Invalid URL %s: %v", targetURL, err)
		return nil, err
	}

	// Check metadata cache first
	if cached, found := cch.Get(normalizedURL); found {
		logrus.Debugf("[GetMetadata] Cache hit for URL: %s", normalizedURL)
		return cached.(*MetaDataResponseItem), nil
	}

	// Parse the URL to extract the domain
	parsedURL, err := urlpkg.Parse(normalizedURL)
	if err != nil {
		logrus.Errorf("[GetMetadata] Failed to parse URL %s: %v", normalizedURL, err)
		return nil, err
	}
	domain := parsedURL.Hostname()

	// Check if the domain is in the Chromedp whitelist
	if isWhitelisted(domain) {
		logrus.Debugf("[GetMetadata] Domain %s is whitelisted. Using Chromedp.", domain)
		result, err := scrapeWithChromedp(normalizedURL)
		if err != nil {
			logrus.Errorf("[GetMetadata] Chromedp failed for URL %s: %v", normalizedURL, err)
			return nil, err
		}
		// Cache the result
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	// Attempt to scrape with Colly
	logrus.Debugf("[GetMetadata] Attempting to scrape with Colly for URL: %s", normalizedURL)
	result, err := scrapeWithColly(normalizedURL)
	if err != nil {
		logrus.Errorf("[GetMetadata] Colly scraping failed for URL %s: %v", normalizedURL, err)
		// Since Colly failed, fallback to Chromedp
		logrus.Debugf("[GetMetadata] Falling back to Chromedp for URL: %s", normalizedURL)
		result, err = scrapeWithChromedp(normalizedURL)
		if err != nil {
			logrus.Errorf("[GetMetadata] Chromedp scraping failed for URL %s: %v", normalizedURL, err)
			return nil, err
		}
		// Add domain to Chromedp whitelist for future requests
		addToWhitelist(domain)
		// Cache the result
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	// Determine if Colly extracted sufficient metadata
	if isMetadataComplete(result) {
		logrus.Debugf("[GetMetadata] Colly scraping succeeded for URL: %s", normalizedURL)
		// Cache the result
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	// If metadata is incomplete, fallback to Chromedp
	logrus.Debugf("[GetMetadata] Colly scraping incomplete for URL: %s. Falling back to Chromedp.", normalizedURL)
	result, err = scrapeWithChromedp(normalizedURL)
	if err != nil {
		logrus.Errorf("[GetMetadata] Chromedp scraping failed for URL %s: %v", normalizedURL, err)
		return nil, err
	}

	// Add domain to Chromedp whitelist for future requests
	addToWhitelist(domain)

	// Cache the result
	cch.Set(normalizedURL, result, cache.DefaultExpiration)

	return result, nil
}

// scrapeWithColly uses the Colly library to scrape metadata from static pages.
func scrapeWithColly(targetURL string) (*MetaDataResponseItem, error) {
	// Initialize Colly collector
	c := colly.NewCollector(
		colly.UserAgent(userAgent),
		colly.AllowURLRevisit(),
	)

	// Initialize the result
	result := &MetaDataResponseItem{
		URL:    targetURL,
		Images: []WebImage{},
	}
	result.Domain = getBaseDomain(targetURL)

	// Temporary storage for WebImage
	var tempWebImage WebImage

	// Set up Colly callbacks
	c.OnRequest(func(r *colly.Request) {
		logrus.Debugf("[Colly] Visiting %s", r.URL.String())
		r.Headers.Set("User-Agent", userAgent)
	})

	c.OnHTML("title", func(e *colly.HTMLElement) {
		if strings.TrimSpace(result.Title) == "" {
			result.Title = strings.TrimSpace(e.Text)
		}
	})

	c.OnHTML(`meta[name="description"]`, func(e *colly.HTMLElement) {
		if strings.TrimSpace(result.Description) == "" {
			result.Description = strings.TrimSpace(e.Attr("content"))
		}
	})

	c.OnHTML(`link[rel="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`, func(e *colly.HTMLElement) {
		if strings.TrimSpace(result.Favicon) == "" {
			href := strings.TrimSpace(e.Attr("href"))
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
		if strings.TrimSpace(result.Sitename) == "" {
			result.Sitename = strings.TrimSpace(e.Attr("content"))
		}
	})

	c.OnHTML(`meta[property="og:image"]`, func(e *colly.HTMLElement) {
		tempWebImage.URL = strings.TrimSpace(e.Attr("content"))
	})

	c.OnHTML(`meta[property="og:image:alt"]`, func(e *colly.HTMLElement) {
		tempWebImage.Alt = strings.TrimSpace(e.Attr("content"))
	})

	c.OnHTML(`meta[property="og:image:type"]`, func(e *colly.HTMLElement) {
		tempWebImage.Type = strings.TrimSpace(e.Attr("content"))
	})

	c.OnHTML(`meta[property="og:image:width"]`, func(e *colly.HTMLElement) {
		width, err := strconv.Atoi(strings.TrimSpace(e.Attr("content")))
		if err == nil {
			tempWebImage.Width = width
		}
	})

	c.OnHTML(`meta[property="og:image:height"]`, func(e *colly.HTMLElement) {
		height, err := strconv.Atoi(strings.TrimSpace(e.Attr("content")))
		if err == nil {
			tempWebImage.Height = height
		}
	})

	c.OnScraped(func(r *colly.Response) {
		if tempWebImage.URL != "" {
			result.Images = append(result.Images, tempWebImage)
			logrus.Debugf("[Colly] Scraping finished for %s", r.Request.URL.String())
		}
	})

	// Visit the target URL
	if err := c.Visit(targetURL); err != nil {
		return nil, err
	}

	return result, nil
}

// scrapeWithChromedp uses Chromedp to scrape metadata from dynamically rendered pages.
func scrapeWithChromedp(targetURL string) (*MetaDataResponseItem, error) {
	// Acquire semaphore to limit concurrency
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
	ctx, cancel = context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	var htmlContent string
	err := chromedp.Run(ctx,
		chromedp.Navigate(targetURL),
		chromedp.WaitVisible("body", chromedp.ByQuery),
		chromedp.OuterHTML("html", &htmlContent, chromedp.ByQuery),
	)
	if err != nil {
		logrus.Error("[Chromedp] Navigation failed:", err)
		return nil, err
	}

	// Parse the rendered HTML using goquery
	result := &MetaDataResponseItem{
		URL:    targetURL,
		Images: []WebImage{},
	}
	result.Domain = getBaseDomain(targetURL)

	if err := parseHTMLWithGoquery(htmlContent, result); err != nil {
		logrus.Errorf("[Chromedp] Failed to parse HTML for URL %s: %v", targetURL, err)
		return nil, err
	}

	return result, nil
}

// parseHTMLWithGoquery parses the HTML content using goquery and fills the result.
func parseHTMLWithGoquery(htmlContent string, result *MetaDataResponseItem) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return err
	}

	// Extract <title>
	doc.Find("title").Each(func(i int, s *goquery.Selection) {
		if strings.TrimSpace(result.Title) == "" {
			result.Title = strings.TrimSpace(s.Text())
		}
	})

	// Extract meta description
	doc.Find(`meta[name="description"]`).Each(func(i int, s *goquery.Selection) {
		if desc, exists := s.Attr("content"); exists && strings.TrimSpace(result.Description) == "" {
			result.Description = strings.TrimSpace(desc)
		}
	})

	// Extract favicon
	doc.Find(`link[rel="icon"], link[rel="shortcut icon"], ` +
		`link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`).Each(func(i int, s *goquery.Selection) {
		if strings.TrimSpace(result.Favicon) == "" {
			if href, exists := s.Attr("href"); exists {
				href = strings.TrimSpace(href)
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
		if sitename, exists := s.Attr("content"); exists && strings.TrimSpace(result.Sitename) == "" {
			result.Sitename = strings.TrimSpace(sitename)
		}
	})

	// Extract OpenGraph images
	doc.Find(`meta[property="og:image"]`).Each(func(i int, s *goquery.Selection) {
		img := WebImage{}
		if imgURL, exists := s.Attr("content"); exists {
			img.URL = strings.TrimSpace(imgURL)
		}
		// Extract additional og:image properties
		parent := s.Parent()
		if parent != nil {
			// og:image:alt
			if alt, exists := parent.Find(`meta[property="og:image:alt"]`).Attr("content"); exists {
				img.Alt = strings.TrimSpace(alt)
			}
			// og:image:type
			if imgType, exists := parent.Find(`meta[property="og:image:type"]`).Attr("content"); exists {
				img.Type = strings.TrimSpace(imgType)
			}
			// og:image:width
			if widthStr, exists := parent.Find(`meta[property="og:image:width"]`).Attr("content"); exists {
				if width, err := strconv.Atoi(strings.TrimSpace(widthStr)); err == nil {
					img.Width = width
				}
			}
			// og:image:height
			if heightStr, exists := parent.Find(`meta[property="og:image:height"]`).Attr("content"); exists {
				if height, err := strconv.Atoi(strings.TrimSpace(heightStr)); err == nil {
					img.Height = height
				}
			}
		}
		if img.URL != "" {
			result.Images = append(result.Images, img)
		}
	})

	// Fallback to og:title if sitename is still empty
	if strings.TrimSpace(result.Sitename) == "" {
		doc.Find(`meta[property="og:title"]`).Each(func(i int, s *goquery.Selection) {
			if sitename, exists := s.Attr("content"); exists && strings.TrimSpace(result.Sitename) == "" {
				result.Sitename = strings.TrimSpace(sitename)
			}
		})
	}

	return nil
}

// isMetadataComplete checks if the metadata contains at least one essential field.
func isMetadataComplete(metadata *MetaDataResponseItem) bool {
	logrus.Debugf("[isMetadataComplete] Title: %s, Description: %s, Sitename: %s, Images: %d", metadata.Title, metadata.Description, metadata.Sitename, len(metadata.Images))
	return strings.TrimSpace(metadata.Title) != "" ||
		strings.TrimSpace(metadata.Description) != "" ||
		strings.TrimSpace(metadata.Sitename) != "" &&
			len(metadata.Images) > 0
}

// normalizeURL ensures the URL has a scheme and is properly formatted.
func normalizeURL(rawURL string) (string, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}
	parsed, err := urlpkg.Parse(rawURL)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
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

// requiresJavaScript determines if the given domain likely requires JavaScript rendering.
// This function can be enhanced with more sophisticated logic or configuration.
func requiresJavaScript(domain string) bool {
	whitelistMutex.RLock()
	defer whitelistMutex.RUnlock()
	_, exists := chromedpWhitelist.Get(domain)
	return exists
}

// addToWhitelist adds a domain to the Chromedp whitelist.
func addToWhitelist(domain string) {
	whitelistMutex.Lock()
	defer whitelistMutex.Unlock()
	chromedpWhitelist.Set(domain, true, cache.NoExpiration)
	logrus.Debugf("[Whitelist] Domain %s added to Chromedp whitelist.", domain)
}

// isWhitelisted checks if a domain is in the Chromedp whitelist.
func isWhitelisted(domain string) bool {
	whitelistMutex.RLock()
	defer whitelistMutex.RUnlock()
	_, exists := chromedpWhitelist.Get(domain)
	return exists
}

// ParsePage is a placeholder for future functionality.
// Currently, it does nothing.
func ParsePage(url string) {
	// Future implementation can go here
}
