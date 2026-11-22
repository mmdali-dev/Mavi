package mavi

import (
	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
	"slices"
	"strconv"
	"strings"
	"time"
)

func day(t time.Time) time.Time {
	y, m, d := t.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, zone)
}

// Modern Jalali years 1300..1499 fall within the same Borkowski break interval (1210..1635).
func persian(s string) (time.Time, error) {
	p := strings.Split(digits(s), "/")
	if len(p) != 3 {
		return time.Time{}, invalid()
	}
	y, e := strconv.Atoi(p[0])
	m, e2 := strconv.Atoi(p[1])
	d, e3 := strconv.Atoi(p[2])
	if e != nil || e2 != nil || e3 != nil || y < 1300 || y > 1499 || m < 1 || m > 12 || d < 1 {
		return time.Time{}, invalid()
	}
	start := func(y int) time.Time {
		g := y + 621
		n := y - 1210
		j := -14
		prev := -61
		for _, b := range []int{9, 38, 199, 426, 686, 756, 818, 1111, 1181, 1210} {
			jump := b - prev
			j += jump/33*8 + jump%33/4
			prev = b
		}
		j += n/33*8 + (n%33+3)/4
		l := g/4 - (g/100+1)*3/4 - 150
		return time.Date(g, 3, 20+j-l, 0, 0, 0, 0, zone)
	}
	max := 31
	if m > 6 {
		max = 30
	}
	if m == 12 {
		max = int(start(y+1).Sub(start(y)).Hours()/24+0.5) - 336
	}
	if d > max {
		return time.Time{}, invalid()
	}
	return start(y).AddDate(0, 0, (m-1)*30+min(m-1, 6)+d-1), nil
}
func (s *Server) dashboard(c fiber.Ctx) error {
	d := day(s.cfg.Now())
	a, b := stamp(d), stamp(d.AddDate(0, 0, 1))
	r, e := one(s.DB, "SELECT (SELECT count(*) FROM orders WHERE created>=? AND created<?) orders,coalesce(sum(delivery_status='pending'),0) pending,coalesce(sum(payment_status='unpaid' AND delivery_status<>'canceled'),0) unpaid,(SELECT coalesce(sum(CASE action WHEN 'pay' THEN amount WHEN 'refund' THEN -amount ELSE 0 END),0) FROM events WHERE action IN ('pay','refund') AND created>=? AND created<?) total FROM units JOIN orders ON order_id=orders.id WHERE orders.created>=? AND orders.created<?", a, b, a, b, a, b)
	return reply(c, r, e)
}
func page(c fiber.Ctx, allowed string) (int, int, error) {
	for k, v := range c.Queries() {
		if !slices.Contains(strings.Fields("page limit "+allowed), k) || len(v) > 200 {
			return 0, 0, invalid()
		}
	}
	p, e := strconv.Atoi(c.Query("page", "1"))
	l, f := strconv.Atoi(c.Query("limit", "20"))
	if e != nil || f != nil || p < 1 || p > 1000000 || l < 1 || l > 100 {
		return 0, 0, invalid()
	}
	return p, l, nil
}
func paged(c fiber.Ctx, db *gorm.DB, q, order string, args []any, p, l int) error {
	var n int64
	var a []map[string]any
	e := db.Transaction(func(tx *gorm.DB) error {
		if e := tx.Raw("SELECT count(*) FROM ("+q+")", args...).Scan(&n).Error; e != nil {
			return e
		}
		var e error
		a, e = rows(tx, q+" ORDER BY "+order+" LIMIT ? OFFSET ?", append(args, l, (p-1)*l)...)
		return e
	})
	return reply(c, fiber.Map{"items": a, "total": n, "page": p, "limit": l}, e)
}

const unitQ = "SELECT u.id,u.name,u.amount,u.delivery_status,u.payment_status,o.code,o.table_number,o.created FROM units u JOIN orders o ON o.id=u.order_id"

func (s *Server) units(c fiber.Ctx) error {
	p, l, e := page(c, "table payment delivery q period from to")
	if e != nil {
		return e
	}
	q := unitQ + " WHERE 1=1"
	a := []any{}
	add := func(cond string, v any) { q += " AND " + cond; a = append(a, v) }
	if t := c.Query("table"); t != "" {
		i, e := strconv.Atoi(digits(t))
		if e != nil || !s.tables[i] {
			return invalid()
		}
		add("o.table_number=?", i)
	}
	for _, f := range []struct{ k, opts string }{{"delivery", "pending delivered canceled"}, {"payment", "unpaid paid refunded"}} {
		if v := c.Query(f.k); v != "" {
			if !strings.Contains(" "+f.opts+" ", " "+v+" ") || strings.Contains(v, " ") {
				return invalid()
			}
			add("u."+f.k+"_status=?", v)
		}
	}
	if v := c.Query("q"); v != "" {
		q += " AND (instr(u.name,?)>0 OR instr(o.code,upper(?))>0)"
		a = append(a, v, v)
	}
	d := day(s.cfg.Now())
	var lo, hi time.Time
	switch c.Query("period", "all") {
	case "today":
		lo = d
		hi = d.AddDate(0, 0, 1)
	case "yesterday":
		lo = d.AddDate(0, 0, -1)
		hi = d
	case "week":
		lo = d.AddDate(0, 0, -6)
		hi = d.AddDate(0, 0, 1)
	case "all":
	case "custom":
		if v := c.Query("from"); v != "" {
			lo, e = persian(v)
			if e != nil {
				return e
			}
		}
		if v := c.Query("to"); v != "" {
			hi, e = persian(v)
			if e != nil {
				return e
			}
			hi = hi.AddDate(0, 0, 1)
		}
		if !hi.IsZero() && !lo.Before(hi) {
			return invalid()
		}
	default:
		return invalid()
	}
	if !lo.IsZero() {
		add("o.created>=?", stamp(lo))
	}
	if !hi.IsZero() {
		add("o.created<?", stamp(hi))
	}
	return paged(c, s.DB, q, "o.created DESC,u.id DESC", a, p, l)
}
func (s *Server) detail(c fiber.Ctx) error {
	id := c.Params("id")
	r, e := one(s.DB, strings.Replace(unitQ, " FROM", ",o.mobile,o.note,o.source FROM", 1)+" WHERE u.id=?", id)
	if e != nil {
		return e
	}
	var history []staffEvent
	e = s.DB.Raw("SELECT action,created,actor,reason,amount,batch FROM events WHERE unit_id=? ORDER BY id", id).Scan(&history).Error
	r["events"] = history
	return reply(c, r, e)
}
func (s *Server) customers(c fiber.Ctx) error {
	p, l, e := page(c, "q")
	if e != nil {
		return e
	}
	v := phone(c.Query("q"))
	if strings.Trim(v, "0123456789") != "" {
		return invalid()
	}
	return paged(c, s.DB, "SELECT min(id) id,mobile,count(*) orders,max(created) last_order FROM orders WHERE mobile<>'' AND instr(mobile,?)>0 GROUP BY mobile", "last_order DESC,id DESC", []any{v}, p, l)
}
func (s *Server) history(c fiber.Ctx) error {
	p, l, e := page(c, "")
	if e != nil {
		return e
	}
	r, e := one(s.DB, "SELECT mobile FROM orders WHERE id=? AND mobile<>'' AND id=(SELECT min(o.id) FROM orders o WHERE o.mobile=orders.mobile)", c.Params("id"))
	if e != nil {
		return e
	}
	var count int64
	var items []map[string]any
	e = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Raw("SELECT count(*) FROM orders WHERE mobile=?", r["mobile"]).Scan(&count).Error; err != nil {
			return err
		}
		var err error
		items, err = rows(tx, `WITH recent AS (
SELECT id,code,created,source,table_number FROM orders WHERE mobile=? ORDER BY created DESC,id DESC LIMIT ? OFFSET ?
) SELECT recent.*,count(u.id) units,sum(u.amount) total FROM recent LEFT JOIN units u ON u.order_id=recent.id
GROUP BY recent.id ORDER BY recent.created DESC,recent.id DESC`, r["mobile"], l, (p-1)*l)
		return err
	})
	return reply(c, fiber.Map{"items": items, "total": count, "page": p, "limit": l}, e)
}
