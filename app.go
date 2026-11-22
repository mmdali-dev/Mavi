package mavi

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/glebarez/sqlite"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed Mavi-Hookah.html mavi-staff.html
var defaultPages embed.FS

// PrepareDeployment installs missing files only. Once installed, HTML is served from disk.
func PrepareDeployment() error {
	for _, name := range []string{"Mavi-Hookah.html", "mavi-staff.html", ".env"} {
		info, err := os.Stat(name)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", name)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var data []byte
		if name == ".env" {
			data = []byte("MAVI_PORT=8080\nMAVI_DATA=./data\nMAVI_TABLES=1-12,15\nMAVI_SEED=1\nMAVI_STAFF_PATH=/" + rand.Text() + "\n# For the supplied local Caddy proxy:\n# MAVI_TRUSTED_PROXY=127.0.0.1\n")
		} else {
			data, err = defaultPages.ReadFile(name)
			if err != nil {
				return err
			}
		}
		if err = installMissing(name, data); err != nil {
			return err
		}
	}
	return nil
}

// Publish a complete file atomically without replacing an existing file.
func installMissing(name string, data []byte) error {
	temp, err := os.CreateTemp(".", ".mavi-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(data)
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(temp.Name(), name); errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

type Config struct {
	Dir, Staff, Tables, TrustedProxy string
	Seed                             bool
	Now                              func() time.Time
}
type Server struct {
	App       *fiber.App
	DB        *gorm.DB
	cfg       Config
	tables    map[int]bool
	menuMu    sync.Mutex
	menuJSON  []byte
	menuUntil time.Time
	uploadMu  sync.Mutex
}

var zone, _ = time.LoadLocation("Asia/Tehran")
var tokenRE = regexp.MustCompile("^[A-Za-z0-9_-]{22,100}$")

func token() string {
	var b [16]byte
	rand.Read(b[:])
	return base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding).EncodeToString(b[:])
}
func digits(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '۰' && r <= '۹':
			return r - '۰' + '0'
		case r >= '٠' && r <= '٩':
			return r - '٠' + '0'
		}
		return r
	}, s)
}
func norm(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("ي", "ی", "ك", "ک", "‌", " ").Replace(digits(s))), " ")
}
func stamp(t time.Time) string     { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func fail(n int, msg string) error { return fiber.NewError(n, msg) }

type invalidField struct{ key, msg string }

func (e invalidField) Error() string { return e.msg }
func fieldError(k, msg string) error { return invalidField{k, msg} }
func invalid() error                 { return fail(422, "مقدار واردشده معتبر نیست.") }
func missing() error                 { return fail(404, "یافت نشد.") }
func conflict() error {
	return fail(409, "اطلاعات تغییر کرده است؛ دوباره بررسی و تأیید کنید.")
}
func body(c fiber.Ctx, v any) error {
	m, _, _ := mime.ParseMediaType(c.Get("Content-Type"))
	if m != "application/json" {
		return fail(415, "نوع درخواست باید JSON باشد.")
	}
	raw := c.BodyRaw()
	if len(raw) > 32768 {
		return fail(413, "درخواست بیش از حد بزرگ است.")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) || d.Decode(v) != nil || d.Decode(new(any)) != io.EOF {
		return fail(400, "ساختار درخواست معتبر نیست.")
	}
	return nil
}
func rows(db *gorm.DB, q string, args ...any) ([]map[string]any, error) {
	out := []map[string]any{}
	err := db.Raw(q, args...).Scan(&out).Error
	return out, err
}
func one(db *gorm.DB, q string, args ...any) (map[string]any, error) {
	a, e := rows(db, q, args...)
	if e != nil {
		return nil, e
	}
	if len(a) == 0 {
		return nil, missing()
	}
	return a[0], nil
}
func reply(c fiber.Ctx, v any, e error) error {
	if e != nil {
		return e
	}
	return c.JSON(v)
}
func tableSet(raw string) (map[int]bool, error) {
	out := map[int]bool{}
	for _, p := range strings.Split(raw, ",") {
		lo, hi, ok := strings.Cut(strings.TrimSpace(p), "-")
		a, e := strconv.Atoi(lo)
		b := a
		if ok {
			b, e = strconv.Atoi(hi)
		}
		if e != nil || a < 1 || b < a || b > 9999 {
			return nil, errors.New("invalid table configuration")
		}
		for i := a; i <= b; i++ {
			out[i] = true
		}
	}
	return out, nil
}

const contentPolicy = "default-src 'self'; script-src %s; style-src 'self' 'unsafe-inline'; font-src 'self' data:; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Cache only immutable responses. Replacing the local file reloads it on the next request.
func diskPage(path string, public bool) fiber.Handler {
	var mu sync.Mutex
	var last os.FileInfo
	var raw, compressed []byte
	var policy, etag string
	return func(c fiber.Ctx) error {
		mu.Lock()
		defer mu.Unlock()
		st, err := os.Stat(path)
		if err != nil {
			return errors.New("page unavailable")
		}
		if last == nil || !os.SameFile(last, st) || last.Size() != st.Size() || !last.ModTime().Equal(st.ModTime()) {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// Only scripts explicitly marked by the local template get permission to execute.
			const open = `<script nonce="__MAVI_NONCE__">`
			var hashes []string
			for rest := data; ; {
				i := bytes.Index(rest, []byte(open))
				if i < 0 {
					break
				}
				rest = rest[i+len(open):]
				j := bytes.Index(rest, []byte("</script>"))
				if j < 0 {
					return errors.New("invalid page script")
				}
				script := bytes.ReplaceAll(rest[:j], []byte("\r\n"), []byte("\n"))
				script = bytes.ReplaceAll(script, []byte("\r"), []byte("\n"))
				h := sha256.Sum256(script)
				hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(h[:])+"'")
				rest = rest[j+len("</script>"):]
			}
			src := strings.Join(hashes, " ")
			if src == "" {
				src = "'none'"
			}
			data = bytes.ReplaceAll(data, []byte(` nonce="__MAVI_NONCE__"`), nil)
			var buf bytes.Buffer
			w, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
			if _, err = w.Write(data); err != nil {
				return err
			}
			if err = w.Close(); err != nil {
				return err
			}
			raw, compressed, last = data, buf.Bytes(), st
			policy = fmt.Sprintf(contentPolicy, src)
			etag = fmt.Sprintf(`W/"%x"`, sha256.Sum256(data))
		}
		c.Set("Content-Security-Policy", policy)
		c.Set("Content-Type", "text/html; charset=utf-8")
		c.Vary("Accept-Encoding")
		if public {
			c.Set("Cache-Control", "public, no-cache")
			c.Set("ETag", etag)
			for _, v := range strings.Split(c.Get("If-None-Match"), ",") {
				v = strings.TrimSpace(v)
				if v == "*" || strings.TrimPrefix(v, "W/") == strings.TrimPrefix(etag, "W/") {
					return c.Status(304).Send(nil)
				}
			}
		}
		if c.Get("Accept-Encoding") != "" && c.AcceptsEncodings("gzip", "identity") == "gzip" {
			c.Set("Content-Encoding", "gzip")
			return c.Send(compressed)
		}
		return c.Send(raw)
	}
}

func New(cfg Config) (*Server, error) {
	if cfg.Staff != "" && (!regexp.MustCompile("^/[A-Za-z0-9_-]{1,100}$").MatchString(cfg.Staff) || strings.Contains("|/menu|/tracking|/about|/api|/assets|/admin|", "|"+cfg.Staff+"|")) {
		return nil, errors.New("invalid staff configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ts, e := tableSet(cfg.Tables)
	if e != nil {
		return nil, e
	}
	var proxies []string
	if cfg.TrustedProxy != "" {
		if _, err := netip.ParseAddr(cfg.TrustedProxy); err != nil {
			return nil, errors.New("invalid trusted proxy")
		}
		proxies = []string{cfg.TrustedProxy}
	}
	db, e := openDB(cfg)
	if e != nil {
		return nil, e
	}
	app := fiber.New(fiber.Config{Immutable: true, IdleTimeout: 15 * time.Second, ReduceMemoryUsage: true, CaseSensitive: true, TrustProxy: true, TrustProxyConfig: fiber.TrustProxyConfig{Proxies: proxies}, ProxyHeader: "X-Real-IP", DisablePreParseMultipartForm: true, BodyLimit: 5 << 20, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, ErrorHandler: func(c fiber.Ctx, err error) error {
		n := 500
		msg := "خطای سرور؛ دوباره تلاش کنید."
		fields := fiber.Map{}
		var validation invalidField
		if errors.As(err, &validation) {
			n = 422
			msg = validation.msg
			fields[validation.key] = msg
		}
		var f *fiber.Error
		if errors.As(err, &f) {
			n = f.Code
			msg = f.Message
			if n == 404 || n == 405 {
				n = 404
				msg = "یافت نشد."
			}
		}
		c.Set("Cache-Control", "no-store")
		return c.Status(n).JSON(fiber.Map{"error": fiber.Map{"code": fmt.Sprintf("http_%d", n), "message": msg, "fields": fields}})
	}})
	s := &Server{App: app, DB: db, cfg: cfg, tables: ts}
	app.Use(recover.New(), func(c fiber.Ctx) error {
		c.Set("X-Request-ID", token())
		c.Set("Referrer-Policy", "no-referrer")
		c.Set("X-Content-Type-Options", "nosniff")
		c.Set("Cache-Control", "no-store")
		c.Set("Content-Security-Policy", fmt.Sprintf(contentPolicy, "'none'"))
		return c.Next()
	})
	app.Use("/api/public/v1", limiter.New(limiter.Config{Max: 20, Expiration: time.Minute, LimitReached: func(c fiber.Ctx) error {
		c.Set("Access-Control-Allow-Origin", "*")
		return fail(429, "تعداد درخواست زیاد است؛ یک دقیقه صبر کنید.")
	}}))
	app.Use("/api/public/v1", cors.New(cors.Config{
		AllowOrigins: []string{"*"}, AllowMethods: []string{"GET", "HEAD", "POST"},
		AllowHeaders: []string{"Content-Type", "Idempotency-Key"}, ExposeHeaders: []string{"Retry-After"}, MaxAge: 600,
	}))
	app.Use(func(c fiber.Ctx) error {
		if c.Method() != "GET" && c.Method() != "HEAD" {
			if enc := c.Get("Content-Encoding"); enc != "" && enc != "identity" {
				return fail(415, "فشرده‌سازی درخواست پشتیبانی نمی‌شود.")
			}
			if cfg.Staff != "" && strings.HasPrefix(c.Path(), cfg.Staff+"/") {
				origin, host, site := c.Get("Origin"), string(c.Request().Host()), c.Get("Sec-Fetch-Site")
				if site == "cross-site" || site == "same-site" || (origin != "" && origin != "http://"+host && origin != "https://"+host) {
					return fail(403, "مبدأ درخواست مجاز نیست.")
				}
			}
		}
		return c.Next()
	})
	publicPage := diskPage("Mavi-Hookah.html", true)
	app.Get("/", func(c fiber.Ctx) error {
		q := c.Query("table")
		p := "/menu"
		if q != "" {
			p += "?table=" + url.QueryEscape(q)
		}
		return c.Redirect().Status(302).To(p)
	})
	for _, p := range []string{"/menu", "/tracking", "/about"} {
		app.Get(p, publicPage)
	}
	pub := app.Group("/api/public/v1")
	pub.Get("/menu", s.menu)
	pub.Get("/media/:id", s.media(false))
	pub.Post("/orders", s.order(false))
	pub.Post("/tracking", s.track)
	if cfg.Staff != "" {
		app.Get(cfg.Staff, diskPage("mavi-staff.html", false))
		g := app.Group(cfg.Staff + "/api/v1")
		g.Post("/orders", s.order(true))
		g.Get("/units", s.units)
		g.Get("/units/:id", s.detail)
		g.Post("/units/actions/preview", s.action(false))
		g.Post("/units/actions/commit", s.action(true))
		g.Get("/dashboard", s.dashboard)
		g.Get("/customers", s.customers)
		g.Get("/customers/:id/orders", s.history)
		g.Post("/media", s.upload)
		g.Get("/media/:id", s.media(true))
		g.Put("/banners/order", s.reorder)
		for _, k := range []string{"products", "categories", "banners"} {
			for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
				p := "/" + k
				if m == "PUT" || m == "DELETE" {
					p += "/:id"
				}
				g.Add([]string{m}, p, s.catalog(k))
				if m == "GET" {
					g.Get(p+"/:id", s.catalog(k))
				}
			}
		}
	}
	if e = s.Clean(); e != nil {
		s.Close()
		return nil, e
	}
	return s, nil
}
func (s *Server) Close() { db, _ := s.DB.DB(); db.Close() }

func openDB(cfg Config) (*gorm.DB, error) {
	if e := os.MkdirAll(filepath.Join(cfg.Dir, "media"), 0700); e != nil {
		return nil, e
	}
	db, e := gorm.Open(sqlite.Open(filepath.Join(cfg.Dir, "mavi.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return nil, e
	}
	pool, _ := db.DB()
	pool.SetMaxOpenConns(1)
	e = db.Transaction(func(tx *gorm.DB) error {
		var version int
		if err := tx.Raw("PRAGMA user_version").Scan(&version).Error; err != nil {
			return err
		}
		if version == 4 {
			return nil
		}
		if version < 0 || version > 3 {
			return errors.New("unsupported migration")
		}
		if version == 0 {
			if err := tx.Exec(schemaV1).Error; err != nil {
				return err
			}
			if cfg.Seed {
				if err := tx.Exec(initialCatalog).Error; err != nil {
					return err
				}
			}
		}
		if version < 2 {
			if err := tx.Exec(`CREATE INDEX orders_table_created ON orders(table_number,created DESC,id DESC);
CREATE INDEX products_image ON products(image,category_id) WHERE image IS NOT NULL;
CREATE INDEX banners_image ON banners(image,active);
CREATE INDEX events_finance ON events(created,action,amount) WHERE action IN ('pay','refund');
DROP INDEX events_day;
INSERT INTO migrations VALUES(2);
PRAGMA user_version=2;`).Error; err != nil {
				return err
			}
		}
		if version < 3 {
			if err := tx.Exec(`ALTER TABLE categories ADD COLUMN color TEXT NOT NULL DEFAULT '#4670b8';
INSERT INTO migrations VALUES(3);
PRAGMA user_version=3;`).Error; err != nil {
				return err
			}
		}
		return tx.Exec(`CREATE INDEX orders_mobile_created ON orders(mobile,created DESC,id DESC);
CREATE INDEX media_created ON media(created);
INSERT INTO migrations VALUES(4);
PRAGMA user_version=4;`).Error
	})
	if e != nil {
		pool.Close()
		return nil, e
	}
	return db, nil
}

const schemaV1 = `
CREATE TABLE IF NOT EXISTS migrations(version INTEGER PRIMARY KEY);
CREATE TABLE categories(id INTEGER PRIMARY KEY, name TEXT NOT NULL, normalized TEXT NOT NULL UNIQUE, active BOOLEAN NOT NULL, position INTEGER NOT NULL);
CREATE TABLE media(id TEXT PRIMARY KEY, mime TEXT NOT NULL, created TEXT NOT NULL);
CREATE TABLE products(id INTEGER PRIMARY KEY, category_id INTEGER NOT NULL REFERENCES categories(id), name TEXT NOT NULL, price INTEGER NOT NULL CHECK(price BETWEEN 1 AND 1000000000), discount INTEGER CHECK(discount>0 AND discount<price), description TEXT NOT NULL, ingredients TEXT NOT NULL, image TEXT REFERENCES media(id), featured BOOLEAN NOT NULL, available BOOLEAN NOT NULL, archived BOOLEAN NOT NULL DEFAULT 0, position INTEGER NOT NULL);
CREATE INDEX products_category ON products(category_id);
CREATE TABLE banners(id INTEGER PRIMARY KEY, name TEXT NOT NULL, image TEXT NOT NULL REFERENCES media(id), alt TEXT NOT NULL, active BOOLEAN NOT NULL, position INTEGER NOT NULL);
CREATE TABLE orders(id INTEGER PRIMARY KEY, table_number INTEGER NOT NULL, mobile TEXT NOT NULL, created TEXT NOT NULL, source TEXT NOT NULL CHECK(source IN ('web','manual')), note TEXT NOT NULL, code TEXT NOT NULL UNIQUE, attempt TEXT NOT NULL, digest TEXT NOT NULL, UNIQUE(source,attempt));
CREATE INDEX orders_created ON orders(created,id);
CREATE INDEX orders_mobile ON orders(mobile,id);
CREATE TABLE units(id INTEGER PRIMARY KEY, order_id INTEGER NOT NULL REFERENCES orders(id), product_id INTEGER NOT NULL REFERENCES products(id), name TEXT NOT NULL, amount INTEGER NOT NULL CHECK(amount>0), delivery_status TEXT NOT NULL DEFAULT 'pending' CHECK(delivery_status IN ('pending','delivered','canceled')), payment_status TEXT NOT NULL DEFAULT 'unpaid' CHECK(payment_status IN ('unpaid','paid','refunded')));
CREATE INDEX units_order ON units(order_id,id);
CREATE INDEX units_product ON units(product_id);
CREATE TABLE events(id INTEGER PRIMARY KEY, unit_id INTEGER NOT NULL REFERENCES units(id), action TEXT NOT NULL, created TEXT NOT NULL, actor TEXT NOT NULL, amount INTEGER NOT NULL, reason TEXT NOT NULL, batch TEXT NOT NULL);
CREATE INDEX events_unit ON events(unit_id,id);
CREATE INDEX events_day ON events(created,action);
CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'immutable'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'immutable'); END;
INSERT INTO migrations VALUES(1);
`

const initialCatalog = `
INSERT INTO categories VALUES(1,'قهوه','قهوه',1,1),(2,'دسر','دسر',1,2),(3,'نوشیدنی سرد','نوشیدنی سرد',1,3);
INSERT INTO products(category_id,name,price,description,ingredients,featured,available,position) VALUES(1,'لاته ماوی',145000,'اسپرسو و شیر بخارداده','شیر، قهوه',1,1,1),(2,'چیزکیک سن‌سباستین',185000,'با سس میوه‌های قرمز','',0,1,2),(3,'کلد برو',165000,'قهوهٔ سرددم','',0,0,3);
`
