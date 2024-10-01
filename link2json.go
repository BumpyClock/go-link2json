package link2json

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	cch               = cache.New(24*time.Hour, 48*time.Hour)
	userAgent         string
	LINK2JSON_DEBUG   bool
	chromedpWhitelist = cache.New(cache.NoExpiration, cache.NoExpiration)
	whitelistMutex    sync.RWMutex
	chromedpSemaphore = make(chan struct{}, 2)
)

func init() {
	LINK2JSON_DEBUG = true
	if value, exists := os.LookupEnv("LINK2JSON_DEBUG"); exists {
		if parsedValue, err := strconv.ParseBool(value); err == nil {
			LINK2JSON_DEBUG = parsedValue
		}
	}
	if LINK2JSON_DEBUG {
		logrus.Info("[Link2Json] Debug mode is enabled. To disable set env LINK2JSON_DEBUG=false.")
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}

	userAgent = "facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)"

	if value, exists := os.LookupEnv("LINK2JSON_USER_AGENT"); exists {
		userAgent = value
	}
}

func GetMetadata(targetURL string) (*MetaDataResponseItem, error) {
	normalizedURL, err := normalizeURL(targetURL)
	if err != nil {
		logrus.Errorf("[link2json] [GetMetadata] Invalid URL %s: %v", targetURL, err)
		return nil, err
	}

	if cached, found := cch.Get(normalizedURL); found {
		logrus.Debugf("[link2json] [GetMetadata] Cache hit for URL: %s", normalizedURL)
		return cached.(*MetaDataResponseItem), nil
	}

	parsedURL, err := urlpkg.Parse(normalizedURL)
	if err != nil {
		logrus.Errorf("[link2json] [GetMetadata] Failed to parse URL %s: %v", normalizedURL, err)
		return nil, err
	}
	domain := parsedURL.Hostname()

	// Attempt to fetch metadata from metalink.dev
	result := &MetaDataResponseItem{
		URL:    normalizedURL,
		Images: []WebImage{},
	}
	if err := fetchMetadataFromMetalink(normalizedURL, result); err == nil {
		logrus.Debugf("[link2json] [GetMetadata] Successfully fetched metadata from metalink.dev for URL: %s", normalizedURL)
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	if isWhitelisted(domain) {
		logrus.Debugf("[link2json] [GetMetadata] Domain %s is whitelisted. Using Chromedp.", domain)
		result, err := scrapeWithChromedp(normalizedURL)
		if err != nil {
			logrus.Errorf("[GetMetadata] Chromedp failed for URL %s: %v", normalizedURL, err)
			return nil, err
		}
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	logrus.Debugf("[link2json] [GetMetadata] Attempting to scrape with Colly for URL: %s", normalizedURL)
	result, err = scrapeWithColly(normalizedURL)
	if err != nil {
		logrus.Errorf("[link2json] [GetMetadata] Colly scraping failed for URL %s: %v", normalizedURL, err)
		logrus.Debugf("[link2json] [GetMetadata] Falling back to Chromedp for URL: %s", normalizedURL)
		result, err = scrapeWithChromedp(normalizedURL)
		if err != nil {
			logrus.Errorf("[link2json] [GetMetadata] Chromedp scraping failed for URL %s: %v", normalizedURL, err)
			return nil, err
		}
		addToWhitelist(domain)
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	if isMetadataComplete(result) {
		logrus.Debugf("[link2json] [GetMetadata] Colly scraping succeeded for URL: %s", normalizedURL)
		cch.Set(normalizedURL, result, cache.DefaultExpiration)
		return result, nil
	}

	logrus.Debugf("[link2json] [GetMetadata] Colly scraping incomplete for URL: %s. Falling back to Chromedp.", normalizedURL)
	result, err = scrapeWithChromedp(normalizedURL)
	if err != nil {
		logrus.Errorf("[link2json] [GetMetadata] Chromedp scraping failed for URL %s: %v", normalizedURL, err)
		return nil, err
	}

	addToWhitelist(domain)
	cch.Set(normalizedURL, result, cache.DefaultExpiration)

	return result, nil
}

func fetchMetadataFromMetalink(targetURL string, result *MetaDataResponseItem) error {
	apiURL := fmt.Sprintf("https://metalink.dev/?url=%s", urlpkg.QueryEscape(targetURL))
	resp, err := http.Get(apiURL)
	if err != nil {
		logrus.Errorf("[link2json] [fetchMetadataFromMetalink] Failed to fetch metadata from metalink.dev for URL %s: %v", targetURL, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logrus.Errorf("[link2json] [fetchMetadataFromMetalink] Non-OK HTTP status: %s", resp.Status)
		return fmt.Errorf("[link2json] non-OK HTTP status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.Errorf("[link2json] [fetchMetadataFromMetalink] Failed to read response body: %v", err)
		return err
	}

	var metalinkResponse struct {
		URL         string     `json:"url"`
		Favicon     *string    `json:"favicon"`
		Title       string     `json:"title"`
		SiteName    *string    `json:"site_name"`
		Description *string    `json:"description"`
		Image       ImageField `json:"image"`
		Video       *string    `json:"video"`
	}

	if err := json.Unmarshal(body, &metalinkResponse); err != nil {
		logrus.Errorf("[link2json] [fetchMetadataFromMetalink] Failed to unmarshal JSON response: %v", err)
		return err
	}

	result.Title = metalinkResponse.Title
	if metalinkResponse.Description != nil {
		result.Description = *metalinkResponse.Description
	}
	if metalinkResponse.Favicon != nil {
		result.Favicon = *metalinkResponse.Favicon
	}
	if metalinkResponse.SiteName != nil {
		result.Sitename = *metalinkResponse.SiteName
	}
	if metalinkResponse.Image.URL != "" {
		result.Images = append(result.Images, WebImage{URL: metalinkResponse.Image.URL})
	}

	return nil
}

type ImageField struct {
	URL string `json:"url"`
}

func (i *ImageField) UnmarshalJSON(data []byte) error {
	var urlString string
	if err := json.Unmarshal(data, &urlString); err == nil {
		i.URL = urlString
		return nil
	}

	var imageObject struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &imageObject); err == nil {
		i.URL = imageObject.URL
		return nil
	}

	return fmt.Errorf("invalid image field")
}

func scrapeWithColly(targetURL string) (*MetaDataResponseItem, error) {
	c := colly.NewCollector(
		colly.UserAgent(userAgent),
		colly.AllowURLRevisit(),
	)

	result := &MetaDataResponseItem{
		URL:    targetURL,
		Images: []WebImage{},
	}
	result.Domain = getBaseDomain(targetURL)

	var tempWebImage WebImage

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

	if err := c.Visit(targetURL); err != nil {
		return nil, err
	}

	return result, nil
}

func scrapeWithChromedp(targetURL string) (*MetaDataResponseItem, error) {
	chromedpSemaphore <- struct{}{}
	defer func() { <-chromedpSemaphore }()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("blink-settings", "imagesEnabled=false"),
	)

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

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

	logrus.Debugf("[Chromedp] Navigation successful for URL: %s", targetURL)

	result := &MetaDataResponseItem{
		URL:    targetURL,
		Images: []WebImage{},
	}
	result.Domain = getBaseDomain(targetURL)

	if err := parseHTMLWithGoquery(htmlContent, result); err != nil {
		logrus.Errorf("[Chromedp] Failed to parse HTML for URL %s: %v", targetURL, err)
		return nil, err
	}

	logrus.Debugf("[Chromedp] Title: %s, Description: %s, Sitename: %s, Images: %d", result.Title, result.Description, result.Sitename, len(result.Images))

	return result, nil
}

func parseHTMLWithGoquery(htmlContent string, result *MetaDataResponseItem) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return err
	}

	doc.Find("title").Each(func(i int, s *goquery.Selection) {
		if strings.TrimSpace(result.Title) == "" {
			result.Title = strings.TrimSpace(s.Text())
		}
	})

	doc.Find(`meta[name="description"]`).Each(func(i int, s *goquery.Selection) {
		if desc, exists := s.Attr("content"); exists && strings.TrimSpace(result.Description) == "" {
			result.Description = strings.TrimSpace(desc)
		}
	})

	doc.Find(`link[rel="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`).Each(func(i int, s *goquery.Selection) {
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

	doc.Find(`meta[property="og:site_name"]`).Each(func(i int, s *goquery.Selection) {
		if sitename, exists := s.Attr("content"); exists && strings.TrimSpace(result.Sitename) == "" {
			result.Sitename = strings.TrimSpace(sitename)
		}
	})

	doc.Find(`meta[property="og:image"]`).Each(func(i int, s *goquery.Selection) {
		img := WebImage{}
		if imgURL, exists := s.Attr("content"); exists {
			img.URL = strings.TrimSpace(imgURL)
		}
		parent := s.Parent()
		if parent != nil {
			if alt, exists := parent.Find(`meta[property="og:image:alt"]`).Attr("content"); exists {
				img.Alt = strings.TrimSpace(alt)
			}
			if imgType, exists := parent.Find(`meta[property="og:image:type"]`).Attr("content"); exists {
				img.Type = strings.TrimSpace(imgType)
			}
			if widthStr, exists := parent.Find(`meta[property="og:image:width"]`).Attr("content"); exists {
				if width, err := strconv.Atoi(strings.TrimSpace(widthStr)); err == nil {
					img.Width = width
				}
			}
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

	if strings.TrimSpace(result.Sitename) == "" {
		doc.Find(`meta[property="og:title"]`).Each(func(i int, s *goquery.Selection) {
			if sitename, exists := s.Attr("content"); exists && strings.TrimSpace(result.Sitename) == "" {
				result.Sitename = strings.TrimSpace(sitename)
			}
		})
	}

	return nil
}

func isMetadataComplete(metadata *MetaDataResponseItem) bool {
	logrus.Debugf("[isMetadataComplete] Title: %s, Description: %s, Sitename: %s, Images: %d", metadata.Title, metadata.Description, metadata.Sitename, len(metadata.Images))
	return strings.TrimSpace(metadata.Title) != "" ||
		strings.TrimSpace(metadata.Description) != "" ||
		strings.TrimSpace(metadata.Sitename) != "" &&
			len(metadata.Images) > 0
}

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

func getBaseDomain(targetURL string) string {
	parsedURL, err := urlpkg.Parse(targetURL)
	if err != nil {
		logrus.Warnf("[getBaseDomain] Failed to parse URL %s: %v", targetURL, err)
		return ""
	}

	return parsedURL.Scheme + "://" + parsedURL.Host
}

func requiresJavaScript(domain string) bool {
	whitelistMutex.RLock()
	defer whitelistMutex.RUnlock()
	_, exists := chromedpWhitelist.Get(domain)
	return exists
}

func addToWhitelist(domain string) {
	whitelistMutex.Lock()
	defer whitelistMutex.Unlock()
	chromedpWhitelist.Set(domain, true, cache.NoExpiration)
	logrus.Debugf("[Whitelist] Domain %s added to Chromedp whitelist.", domain)
}

func isWhitelisted(domain string) bool {
	whitelistMutex.RLock()
	defer whitelistMutex.RUnlock()
	_, exists := chromedpWhitelist.Get(domain)
	return exists
}
