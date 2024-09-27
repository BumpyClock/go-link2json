package link2json

import (
	"context"
	"net/url"
	"os"
	"strconv"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
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

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"

	if value, exists := os.LookupEnv("LINK2JSON_USER_AGENT"); exists {
		userAgent = value
	}
}

func GetMetadata(url string) (*MetaDataResponseItem, error) {
	// Check cache first
	if cached, found := cch.Get(url); found {
		return cached.(*MetaDataResponseItem), nil
	}

	// Create context
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()

	// Run chromedp tasks
	var title, description, favicon, sitename string
	var imageNodes []*cdp.Node
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitVisible(`body`, chromedp.ByQuery),
		chromedp.Text(`title`, &title, chromedp.NodeVisible),
		chromedp.AttributeValue(`meta[name="description"]`, "content", &description, nil),
		chromedp.AttributeValue(`link[rel="icon"], link[rel="shortcut icon"], link[rel="apple-touch-icon"], link[rel="apple-touch-icon-precomposed"]`, "href", &favicon, nil),
		chromedp.AttributeValue(`meta[property="og:site_name"]`, "content", &sitename, nil),
		chromedp.Nodes(`meta[property="og:image"]`, &imageNodes, chromedp.ByQueryAll),
	)
	if err != nil {
		return nil, err
	}

	// Extract image URLs from nodes
	var images []WebImage
	for _, node := range imageNodes {
		if src, ok := node.Attribute("content"); ok {
			images = append(images, WebImage{URL: src})
		}
	}

	result := &MetaDataResponseItem{
		URL:         url,
		Title:       title,
		Description: description,
		Domain:      getBaseDomain(url),
		Favicon:     favicon,
		Sitename:    sitename,
		Images:      images,
	}

	// Cache the result
	cch.Set(url, result, cache.DefaultExpiration)

	return result, nil
}

func getBaseDomain(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsedURL.Scheme + "://" + parsedURL.Host
}
