package mavi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

type Number int64

func (n *Number) UnmarshalJSON(b []byte) error {
	s := string(b)
	if strings.HasPrefix(s, "\"") {
		if e := json.Unmarshal(b, &s); e != nil {
			return e
		}
	}
	v, e := strconv.ParseInt(digits(s), 10, 64)
	*n = Number(v)
	return e
}

type Item struct {
	Product Number `json:"product_id"`
	Qty     Number `json:"quantity"`
	Price   Number `json:"expected_price"`
}
type OrderInput struct {
	Table  Number `json:"table"`
	Mobile string `json:"mobile"`
	Items  []Item `json:"items"`
}
type Manual struct {
	OrderInput
	Actor string `json:"actor"`
	Note  string `json:"note"`
}

func phone(s string) string {
	s = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(digits(strings.TrimSpace(s)))
	for _, p := range []string{"+98", "0098"} {
		if strings.HasPrefix(s, p) {
			return "0" + s[len(p):]
		}
	}
	return s
}

var mobileRE = regexp.MustCompile("^09[0-9]{9}$")

func validText(s string, n int, required bool) bool {
	return (!required || strings.TrimSpace(s) != "") && utf8.RuneCountInString(s) <= n
}
func (s *Server) order(manual bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		var v Manual
		src := "web"
		var e error
		if manual {
			src = "manual"
			e = body(c, &v)
		} else {
			e = body(c, &v.OrderInput)
			v.Actor = "مشتری"
		}
		if e != nil {
			return e
		}
		key := c.Get("Idempotency-Key")
		v.Actor = strings.TrimSpace(v.Actor)
		v.Note = strings.TrimSpace(v.Note)
		enteredMobile := strings.TrimSpace(v.Mobile) != ""
		v.Mobile = phone(v.Mobile)
		switch {
		case !tokenRE.MatchString(key):
			return fieldError("Idempotency-Key", "کلید ثبت معتبر نیست؛ فرم را دوباره باز کنید.")
		case !s.tables[int(v.Table)]:
			return fieldError("table", "شماره میز معتبر این کافه را وارد کنید.")
		case (!manual || enteredMobile) && !mobileRE.MatchString(v.Mobile):
			return fieldError("mobile", "شماره موبایل معتبر ایران را وارد کنید.")
		case !validText(v.Actor, 80, true):
			return fieldError("actor", "نام کارمند باید بین ۱ تا ۸۰ نویسه باشد.")
		case !validText(v.Note, 500, false):
			return fieldError("note", "یادداشت حداکثر ۵۰۰ نویسه است.")
		case len(v.Items) == 0 || len(v.Items) > 60:
			return fieldError("items", "سبد باید بین ۱ تا ۶۰ واحد داشته باشد.")
		}
		sort.Slice(v.Items, func(i, j int) bool { return v.Items[i].Product < v.Items[j].Product })
		var count Number
		for i, p := range v.Items {
			if p.Product <= 0 || p.Qty < 1 || p.Qty > 30 || p.Price < 1 || p.Price > 1e9 || i > 0 && p.Product == v.Items[i-1].Product {
				return invalid()
			}
			count += p.Qty
		}
		if count > 60 {
			return fieldError("items", "حداکثر ۶۰ واحد در هر سفارش مجاز است.")
		}
		b, _ := json.Marshal(v)
		hash := sha256.Sum256(b)
		digest := hex.EncodeToString(hash[:])
		code := ""
		status := 201
		now := stamp(s.cfg.Now())
		e = s.DB.Transaction(func(tx *gorm.DB) error {
			var old Order
			r := tx.Raw("SELECT code,digest FROM orders WHERE source=? AND attempt=?", src, key).Scan(&old)
			if r.Error != nil {
				return r.Error
			}
			if old.Code != "" {
				if old.Digest != digest {
					return conflict()
				}
				code = old.Code
				status = 200
				return nil
			}
			var next int64
			if err := tx.Raw("SELECT coalesce(max(id),0) FROM orders").Scan(&next).Error; err != nil {
				return err
			}
			if next > 99999999-100 {
				return fail(503, "ظرفیت کدهای پیگیری تکمیل شده است.")
			}
			code = strconv.FormatInt(next+100, 10)
			o := Order{TableNumber: int(v.Table), Mobile: v.Mobile, Created: now, Source: src, Note: v.Note, Code: code, Attempt: key, Digest: digest}
			if err := tx.Table("orders").Create(&o).Error; err != nil {
				return err
			}
			ids := []int64{}
			for _, p := range v.Items {
				ids = append(ids, int64(p.Product))
			}
			var products []struct {
				ID     int64
				Name   string
				Amount int64
			}
			if e := tx.Raw("SELECT p.id,p.name,coalesce(discount,price) amount FROM products p JOIN categories c ON p.category_id=c.id WHERE c.active AND NOT archived AND available AND p.id IN ? ORDER BY p.id", ids).Scan(&products).Error; e != nil {
				return e
			}
			if len(products) != len(ids) {
				return conflict()
			}
			units := make([]OrderUnit, 0, int(count))
			for i, it := range v.Items {
				p := products[i]
				if p.Amount != int64(it.Price) {
					return conflict()
				}
				for range int(it.Qty) {
					units = append(units, OrderUnit{OrderID: o.ID, ProductID: p.ID, Name: p.Name, Amount: p.Amount, DeliveryStatus: "pending", PaymentStatus: "unpaid"})
				}
			}
			if err := tx.Table("units").Create(&units).Error; err != nil {
				return err
			}
			events := make([]OrderEvent, 0, len(units))
			for _, u := range units {
				events = append(events, OrderEvent{UnitID: u.ID, Action: "create", Created: now, Actor: v.Actor})
			}
			return tx.Table("events").Create(&events).Error
		})
		if e != nil {
			return e
		}
		return c.Status(status).JSON(fiber.Map{"code": code, "table": v.Table, "count": count})
	}
}
func event(tx *gorm.DB, id int64, act, now, actor string, amount int64, reason, batch string) error {
	return tx.Exec("INSERT INTO events(unit_id,action,created,actor,amount,reason,batch) VALUES(?,?,?,?,?,?,?)", id, act, now, actor, amount, reason, batch).Error
}
func (s *Server) track(c fiber.Ctx) error {
	var v struct{ Code string }
	if e := body(c, &v); e != nil {
		return e
	}
	code := strings.ToUpper(strings.TrimSpace(digits(v.Code)))
	if len(code) != 26 && (len(code) < 3 || len(code) > 8 || strings.Trim(code, "0123456789") != "") {
		return missing()
	}
	o, e := one(s.DB, "SELECT table_number AS [table],id FROM orders WHERE code=?", code)
	if e != nil {
		return e
	}
	var a []trackedUnit
	e = s.DB.Raw("SELECT name,amount,payment_status,delivery_status FROM units WHERE order_id=? ORDER BY id", o["id"]).Scan(&a).Error
	return reply(c, fiber.Map{"table": o["table"], "units": a}, e)
}

type snapshot struct {
	ID       int64  `json:"id"`
	Amount   int64  `json:"amount"`
	Delivery string `json:"delivery_status" gorm:"column:delivery_status"`
	Payment  string `json:"payment_status" gorm:"column:payment_status"`
}

var proofKey = func() []byte { b := make([]byte, 32); rand.Read(b); return b }()

func sign(act string, v []snapshot) string {
	b, _ := json.Marshal(v)
	h := hmac.New(sha256.New, proofKey)
	h.Write([]byte(act))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *Server) action(commit bool) fiber.Handler {
	return func(c fiber.Ctx) error {
		var v struct {
			Action, Actor, Reason, Proof string
			IDs                          []int64
		}
		if e := body(c, &v); e != nil {
			return e
		}
		v.Actor = strings.TrimSpace(v.Actor)
		v.Reason = strings.TrimSpace(v.Reason)
		set := map[string]string{"deliver": "delivery_status='delivered'", "cancel": "delivery_status='canceled'", "pay": "payment_status='paid'", "refund": "payment_status='refunded'"}[v.Action]
		if set == "" || len(v.IDs) == 0 || len(v.IDs) > 100 || commit && (!validText(v.Actor, 80, true) || !validText(v.Reason, 300, v.Action == "cancel")) {
			return invalid()
		}
		seen := map[int64]bool{}
		for _, id := range v.IDs {
			if id <= 0 || seen[id] {
				return invalid()
			}
			seen[id] = true
		}
		eligible := []snapshot{}
		skipped := []int64{}
		var total int64
		e := s.DB.Transaction(func(tx *gorm.DB) error {
			var all []snapshot
			if e := tx.Raw("SELECT id,amount,delivery_status,payment_status FROM units WHERE id IN ? ORDER BY id", v.IDs).Scan(&all).Error; e != nil {
				return e
			}
			for _, u := range all {
				ok := false
				switch v.Action {
				case "deliver", "cancel":
					ok = u.Delivery == "pending"
				case "pay":
					ok = u.Payment == "unpaid" && u.Delivery != "canceled"
				case "refund":
					ok = u.Payment == "paid" && u.Delivery == "canceled"
				}
				if ok {
					eligible = append(eligible, u)
					total += u.Amount
					delete(seen, u.ID)
				}
			}
			for _, id := range v.IDs {
				if seen[id] {
					skipped = append(skipped, id)
				}
			}
			if !commit {
				return nil
			}
			if len(skipped) > 0 || !hmac.Equal([]byte(sign(v.Action, eligible)), []byte(v.Proof)) {
				return conflict()
			}
			batch := token()
			now := stamp(s.cfg.Now())
			for _, u := range eligible {
				r := tx.Exec("UPDATE units SET "+set+" WHERE id=? AND amount=? AND delivery_status=? AND payment_status=?", u.ID, u.Amount, u.Delivery, u.Payment)
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected != 1 {
					return conflict()
				}
				amt := int64(0)
				if v.Action == "pay" || v.Action == "refund" {
					amt = u.Amount
				}
				if e := event(tx, u.ID, v.Action, now, v.Actor, amt, v.Reason, batch); e != nil {
					return e
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		return c.JSON(fiber.Map{"eligible": eligible, "skipped": skipped, "count": len(eligible), "total": total, "proof": sign(v.Action, eligible)})
	}
}

// Persistence projections. Clients receive only the explicitly selected DTO fields.
type Order struct {
	ID                                                   int64
	Code, Created, Mobile, Source, Note, Attempt, Digest string
	TableNumber                                          int
}
type OrderUnit struct {
	ID, OrderID, ProductID, Amount      int64
	Name, DeliveryStatus, PaymentStatus string
}
type OrderEvent struct {
	ID, UnitID, Amount                    int64
	Action, Created, Actor, Reason, Batch string
}
type trackedUnit struct {
	Name           string `json:"name"`
	Amount         int64  `json:"amount"`
	DeliveryStatus string `json:"delivery_status"`
	PaymentStatus  string `json:"payment_status"`
}
type staffEvent struct {
	Action  string `json:"action"`
	Created string `json:"created"`
	Actor   string `json:"actor"`
	Reason  string `json:"reason"`
	Amount  int64  `json:"amount"`
	Batch   string `json:"batch"`
}
