package mavi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/gofiber/fiber/v3"
	_ "golang.org/x/image/webp"
	"gorm.io/gorm"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const prodCols = "id,category_id,name,price,discount,coalesce(discount,price) final_price,description,ingredients,image,featured,available,archived,position"

func (s *Server) menu(c fiber.Ctx) error {
	s.menuMu.Lock()
	defer s.menuMu.Unlock()
	c.Set("Content-Type", "application/json")
	if s.menuJSON != nil && time.Now().Before(s.menuUntil) {
		return s.sendCachedMenu(c)
	}
	var out = fiber.Map{}
	e := s.DB.Transaction(func(tx *gorm.DB) error {
		for k, q := range map[string]string{"categories": "SELECT id,name,color FROM categories WHERE active ORDER BY position,id", "products": "SELECT " + strings.Replace(prodCols, ",archived", "", 1) + " FROM products WHERE NOT archived AND category_id IN (SELECT id FROM categories WHERE active) ORDER BY position,id", "banners": "SELECT image,alt FROM banners WHERE active ORDER BY position,id"} {
			r, e := rows(tx, q)
			if e != nil {
				return e
			}
			out[k] = r
		}
		return nil
	})
	if e != nil {
		return e
	}
	data, e := json.Marshal(out)
	if e != nil {
		return e
	}
	s.menuJSON = nil
	if len(data) <= 512<<10 {
		s.menuJSON = data
		s.menuGzip = nil
		s.menuETag = fmt.Sprintf(`W/"%x"`, sha256.Sum256(data))
		s.menuUntil = time.Now().Add(30 * time.Second)
		return s.sendCachedMenu(c)
	}
	return c.Send(data)
}

func (s *Server) sendCachedMenu(c fiber.Ctx) error {
	c.Set("Cache-Control", "private, no-cache")
	c.Vary("Accept-Encoding")
	if matchesETag(c, s.menuETag) {
		return c.Status(304).Send(nil)
	}
	if len(s.menuJSON) >= 512 && c.Get("Accept-Encoding") != "" && c.AcceptsEncodings("gzip", "identity") == "gzip" {
		if s.menuGzip == nil {
			var buf bytes.Buffer
			writer := gzip.NewWriter(&buf)
			if _, err := writer.Write(s.menuJSON); err != nil {
				return err
			}
			if err := writer.Close(); err != nil {
				return err
			}
			s.menuGzip = buf.Bytes()
		}
		c.Set("Content-Encoding", "gzip")
		return c.Send(s.menuGzip)
	}
	return c.Send(s.menuJSON)
}

func (s *Server) invalidateMenu() { s.menuMu.Lock(); s.menuJSON = nil; s.menuMu.Unlock() }

var categoryColor = regexp.MustCompile(`^#[a-fA-F0-9]{6}$`)

type Category struct {
	Color    *string
	Name     string
	Active   bool
	Position int
}
type Product struct {
	Archived                       *bool
	Name, Description, Ingredients string
	Image                          *string
	CategoryID                     int64 `json:"category_id"`
	Price                          Number
	Discount                       *Number
	Featured, Available            bool
	Position                       int
}
type Banner struct {
	Name, Image, Alt string
	Active           bool
	Position         int
}

func constraint(e error) error {
	if e != nil && strings.Contains(e.Error(), "constraint") {
		return conflict()
	}
	return e
}
func (s *Server) catalog(k string) fiber.Handler {
	return func(c fiber.Ctx) error {
		cols := map[string]string{"products": prodCols, "categories": "id,name,active,position,color", "banners": "id,name,image,alt,active,position"}[k]
		id := c.Params("id")
		var result any
		status := 200
		if c.Method() == "GET" {
			if id != "" {
				a, e := one(s.DB, "SELECT "+cols+" FROM "+k+" WHERE id=?", id)
				return reply(c, a, e)
			}
			a, e := rows(s.DB, "SELECT "+cols+" FROM "+k+" ORDER BY position,id")
			return reply(c, a, e)
		}
		e := s.DB.Transaction(func(tx *gorm.DB) error {
			if id != "" {
				if _, e := one(tx, "SELECT id FROM "+k+" WHERE id=?", id); e != nil {
					return e
				}
			}
			if c.Method() == "DELETE" {
				var dst struct {
					Target int64 `json:"target_id"`
				}
				if len(c.Body()) > 0 {
					if k == "categories" {
						if e := body(c, &dst); e != nil {
							return e
						}
					} else {
						if e := body(c, &struct{}{}); e != nil {
							return e
						}
					}
				}
				if k == "categories" {
					var n int64
					if e := tx.Raw("SELECT EXISTS(SELECT 1 FROM products WHERE category_id=?)", id).Scan(&n).Error; e != nil {
						return e
					}
					if n > 0 {
						if _, e := one(tx, "SELECT id FROM categories WHERE id=? AND id<>?", dst.Target, id); e != nil {
							return conflict()
						}
						if e := tx.Exec("UPDATE products SET category_id=? WHERE category_id=?", dst.Target, id).Error; e != nil {
							return e
						}
					}
				}
				if k == "products" {
					r := tx.Exec("UPDATE products SET archived=1 WHERE id=? AND EXISTS(SELECT 1 FROM units WHERE product_id=?)", id, id)
					if r.Error != nil {
						return r.Error
					}
					if r.RowsAffected > 0 {
						result = fiber.Map{"result": "archived"}
						return nil
					}
				}
				result = fiber.Map{"result": "deleted"}
				return tx.Exec("DELETE FROM "+k+" WHERE id=?", id).Error
			}
			var fields []string
			var args []any
			switch k {
			case "categories":
				var v Category
				if e := body(c, &v); e != nil {
					return e
				}
				if !validText(norm(v.Name), 100, true) {
					return invalid()
				}
				fields = []string{"name", "normalized", "active", "position"}
				args = []any{strings.TrimSpace(v.Name), strings.ToLower(norm(v.Name)), v.Active, v.Position}
				if v.Color != nil {
					if !categoryColor.MatchString(*v.Color) {
						return fieldError("color", "رنگ دسته‌بندی معتبر نیست.")
					}
					fields = append(fields, "color")
					args = append(args, strings.ToLower(*v.Color))
				}
			case "products":
				var v Product
				if e := body(c, &v); e != nil {
					return e
				}
				if !validText(v.Name, 100, true) || !validText(v.Description, 1000, false) || !validText(v.Ingredients, 500, false) || v.Price < 1 || v.Price > 1e9 || v.Discount != nil && (*v.Discount <= 0 || *v.Discount >= v.Price) {
					return invalid()
				}
				fields = []string{"name", "category_id", "price", "discount", "description", "ingredients", "image", "featured", "available", "position"}
				args = []any{strings.TrimSpace(v.Name), v.CategoryID, v.Price, v.Discount, v.Description, v.Ingredients, v.Image, v.Featured, v.Available, v.Position}
				if v.Archived != nil {
					fields = append(fields, "archived")
					args = append(args, *v.Archived)
				}
			case "banners":
				var v Banner
				if e := body(c, &v); e != nil {
					return e
				}
				if !validText(v.Name, 100, true) || !validText(v.Alt, 200, false) || !tokenRE.MatchString(v.Image) {
					return invalid()
				}
				fields = []string{"name", "image", "alt", "active", "position"}
				args = []any{strings.TrimSpace(v.Name), v.Image, v.Alt, v.Active, v.Position}
			}
			var rid int64
			if id == "" {
				var count int64
				if err := tx.Raw("SELECT count(*) FROM " + k).Scan(&count).Error; err != nil {
					return err
				}
				if count >= map[string]int64{"categories": 250, "products": 2000, "banners": 128}[k] {
					return fail(422, "ظرفیت فهرست پر شده است.")
				}
				status = 201
				returning := tx.Raw("INSERT INTO "+k+"("+strings.Join(fields, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+") RETURNING id", args...).Scan(&rid)
				if returning.Error != nil {
					return returning.Error
				}
			} else {
				args = append(args, id)
				if e := tx.Exec("UPDATE "+k+" SET "+strings.Join(fields, "=?,")+"=? WHERE id=?", args...).Error; e != nil {
					return e
				}
			}
			lookup := any(rid)
			if id != "" {
				lookup = id
			}
			var e error
			result, e = one(tx, "SELECT "+cols+" FROM "+k+" WHERE id=?", lookup)
			return e
		})
		if e != nil {
			return constraint(e)
		}
		s.invalidateMenu()
		return c.Status(status).JSON(result)
	}
}
func (s *Server) reorder(c fiber.Ctx) error {
	var v struct{ IDs []int64 }
	if e := body(c, &v); e != nil {
		return e
	}
	e := s.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		if e := tx.Raw("SELECT count(*) FROM banners").Scan(&n).Error; e != nil {
			return e
		}
		if int(n) != len(v.IDs) {
			return conflict()
		}
		seen := map[int64]bool{}
		for i, id := range v.IDs {
			if seen[id] {
				return invalid()
			}
			seen[id] = true
			r := tx.Exec("UPDATE banners SET position=? WHERE id=?", i, id)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return conflict()
			}
		}
		return nil
	})
	if e == nil {
		s.invalidateMenu()
	}
	return reply(c, fiber.Map{"ids": v.IDs}, e)
}
func (s *Server) upload(c fiber.Ctx) error {
	if !strings.HasPrefix(c.Get("Content-Type"), "multipart/form-data;") {
		return fail(415, "فایل تصویر ارسال کنید.")
	}
	if !s.uploadMu.TryLock() {
		c.Set("Retry-After", "1")
		return fail(429, "یک تصویر در حال پردازش است؛ دوباره تلاش کنید.")
	}
	defer s.uploadMu.Unlock()
	f, e := c.FormFile("file")
	if e != nil {
		return fieldError("file", "فایل تصویر در درخواست پیدا نشد.")
	}
	if f.Size > 4<<20 {
		return fail(413, "تصویر بیش از حد بزرگ است.")
	}
	in, e := f.Open()
	if e != nil {
		return e
	}
	defer in.Close()
	cfg, kind, e := image.DecodeConfig(in)
	if e != nil || (kind != "png" && kind != "jpeg" && kind != "webp") {
		return fieldError("file", "تصویر خوانده نشد؛ یک فایل سالم JPEG، PNG یا WebP بفرستید.")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 8e6 {
		return fieldError("file", "هر ضلع تصویر باید حداکثر ۴۰۹۶ پیکسل و کل تصویر حداکثر ۸ مگاپیکسل باشد.")
	}
	if _, e = in.Seek(0, io.SeekStart); e != nil {
		return e
	}
	if _, _, e = image.Decode(in); e != nil {
		return fieldError("file", "فایل تصویر ناقص یا خراب است؛ یک تصویر دیگر انتخاب کنید.")
	}
	id := token()
	p := filepath.Join(s.cfg.Dir, "media", id)
	if _, e = in.Seek(0, io.SeekStart); e != nil {
		return e
	}
	out, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(out, in)
	closeErr := out.Close()
	if e != nil || closeErr != nil {
		os.Remove(p)
		if e != nil {
			return e
		}
		return closeErr
	}
	if e = s.DB.Exec("INSERT INTO media VALUES(?,?,?)", id, "image/"+kind, stamp(s.cfg.Now())).Error; e != nil {
		os.Remove(p)
		return e
	}
	return c.Status(201).JSON(fiber.Map{"id": id})
}
func (s *Server) media(private bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Params("id")
		if !tokenRE.MatchString(id) {
			return missing()
		}
		q := "SELECT mime FROM media WHERE id=?"
		if !private {
			q += " AND (EXISTS(SELECT 1 FROM products p JOIN categories c ON c.id=p.category_id WHERE p.image=media.id AND NOT p.archived AND c.active) OR EXISTS(SELECT 1 FROM banners WHERE active AND image=media.id))"
		}
		r, e := one(s.DB, q, id)
		if e != nil {
			return e
		}
		f, e := os.Open(filepath.Join(s.cfg.Dir, "media", id))
		if e != nil {
			return missing()
		}
		c.Set("Content-Type", r["mime"].(string))
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		return c.SendStream(f, int(st.Size()))
	}
}
func (s *Server) Clean() error {
	refs, e := rows(s.DB, "DELETE FROM media WHERE created<? AND NOT EXISTS(SELECT 1 FROM banners WHERE image=media.id) AND NOT EXISTS(SELECT 1 FROM products WHERE image=media.id) RETURNING id", stamp(s.cfg.Now().Add(-24*time.Hour)))
	if e != nil {
		return e
	}
	for _, r := range refs {
		if e = os.Remove(filepath.Join(s.cfg.Dir, "media", r["id"].(string))); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	// Recover a file left between file creation/deletion and its database transaction.
	dir := filepath.Join(s.cfg.Dir, "media")
	entries, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, f := range entries {
		if !f.Type().IsRegular() || !tokenRE.MatchString(f.Name()) {
			continue
		}
		st, e := f.Info()
		if e != nil {
			return e
		}
		if st.ModTime().After(s.cfg.Now().Add(-24 * time.Hour)) {
			continue
		}
		var n int64
		if e = s.DB.Raw("SELECT count(*) FROM media WHERE id=?", f.Name()).Scan(&n).Error; e != nil {
			return e
		}
		if n == 0 {
			if e = os.Remove(filepath.Join(dir, f.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}
