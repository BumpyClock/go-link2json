package link2json

import (
	URL "net/url"
	"os"
	"strconv"

	"github.com/gocolly/colly"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
)

var (
	cch             = cache.New(cache.NoExpiration, cache.NoExpiration)
	userAgent       string
	LINK2JSON_DEBUG bool
)

func init() {
	LINK2JSON_DEBUG = true
	if value, exists := os.LookupEnv("LINK2JSON_DEBUG"); exists {
		if parsedValue, err := strconv.ParseBool(value); err == nil {
			LINK2JSON_DEBUG = parsedValue
		}
	}
	if LINK2JSON_DEBUG {
		logrus.Info("[Link2Json] Debug mode is enabled. To disable set env LINK2JSON_DEBUG=release.")
		logrus.SetLevel(logrus.DebugLevel)
	}

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	if value, exists := os.LookupEnv("LINK2JSON_USER_AGENT"); exists {
		userAgent = value
	}

}

func GetMetadata(url string) (*MetaDataResponseItem, error) {

	// Check cache first
	if cached, found := cch.Get(url); found {
		return cached.(*MetaDataResponseItem), nil
	}

	c := colly.NewCollector()
	c.OnRequest(func(r *colly.Request) {
		logrus.Debug("Visiting", r.URL)
		r.Headers.Set("User-Agent", userAgent)
	})
	result := &MetaDataResponseItem{URL: url, Images: []WebImage{}}
	result.Domain = getBaseDomain(url)
	webImage := WebImage{}

	c.OnHTML("title", func(e *colly.HTMLElement) {
		if result.Title == "" {
			result.Title = e.Text
		}
	})
	c.OnHTML(`meta[name="description"]`, func(e *colly.HTMLElement) {
		result.Description = e.Attr("content")
	})
	c.OnHTML(`link[rel="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`, func(e *colly.HTMLElement) {
		if result.Favicon == "" {
			href := e.Attr("href")
			logrus.Debug("[Link2JSON] Favicon found", href)
			parsedURL, err := URL.Parse(href)
			if err != nil || !parsedURL.IsAbs() {
				result.Favicon = result.Domain + href
			} else {
				result.Favicon = href
			}
		}
	})
	c.OnHTML(`meta[property="og:site_name"]`, func(e *colly.HTMLElement) {
		result.Sitename = e.Attr("content")
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
		logrus.Debug("[GetMetaData] Scraping finished", url)
	})

	// Handle visiting the URL
	err := c.Visit(url)
	if err != nil {
		logrus.Debug("[GetMetaData] Failed to visit URL: ", url)
		logrus.Error("[GetMetaData] Failed to visit URL: ", err)
		return nil, err
	}

	if result.Sitename == "" {
		c2 := colly.NewCollector()
		logrus.Debug("Visiting", result.Domain)
		c2.OnHTML(`meta[property="og:title"]`, func(e *colly.HTMLElement) {
			result.Sitename = e.Attr("content")
		})

		err = c2.Visit(result.Domain)
		if err != nil {
			// Log the error but do not return it, allowing the function to proceed
			logrus.Error("[GetMetaData] Failed to visit base domain: ", err)
			// Do not return here, allowing the function to continue
		}
	}

	// Cache the result
	cch.Set(url, result, cache.DefaultExpiration)

	return result, nil
}

func ParsePage(url string) {

}

func getBaseDomain(url string) string {
	parsedURL, err := URL.Parse(url)
	if err != nil {
		return ""
	}

	return parsedURL.Scheme + "://" + parsedURL.Host
}
