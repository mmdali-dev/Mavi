package mavi

import (
	"embed"
	"path"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"
)

//go:embed webassets/*
var assetFiles embed.FS

var bundledAssets = sync.OnceValues(func() (map[string][]byte, error) {
	entries, err := assetFiles.ReadDir("webassets")
	if err != nil {
		return nil, err
	}
	assets := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		data, err := assetFiles.ReadFile("webassets/" + name)
		if err != nil {
			return nil, err
		}
		assets[name] = data
	}
	return assets, nil
})

func pageWithAssets(page fiber.Handler, staff bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		name := c.Query("asset")
		if name == "" {
			return page(c)
		}
		assets, err := bundledAssets()
		if err != nil {
			return err
		}
		data, ok := assets[name]
		if !ok || strings.HasPrefix(name, "staff-") != staff {
			return missing()
		}
		switch path.Ext(name) {
		case ".woff2":
			c.Set("Content-Type", "font/woff2")
		case ".webp":
			c.Set("Content-Type", "image/webp")
		case ".png":
			c.Set("Content-Type", "image/png")
		}
		if staff {
			c.Set("Cache-Control", "private, max-age=31536000, immutable")
		} else {
			c.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		return c.Send(data)
	}
}
