package transforms

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/kensac/orbit/internal/harness"
)

// Month is one calendar month of Sure transactions, summed.
type Month struct {
	Month string `json:"month"`
	// Transactions counts the month's transactions that are not transfers.
	Transactions int `json:"transactions"`
	// Transfers counts the transfers left out of every sum: a move between
	// the person's own accounts (a card payment among them) is not income
	// or spending.
	Transfers  int        `json:"transfers"`
	Income     string     `json:"income"`
	Spending   string     `json:"spending"`
	Net        string     `json:"net"`
	Categories []Category `json:"categories"`
}

// Category is one category's share of a month.
type Category struct {
	Category string `json:"category"`
	Spending string `json:"spending"`
	Income   string `json:"income"`
	Count    int    `json:"count"`
}

// Uncategorized names transactions without a category.
const Uncategorized = "Uncategorized"

// sureMonthly sums Sure's /api/v1/transactions records by month and
// category: {"months": [...], "transactions": n}, oldest month first, each
// month's categories by spending, largest first. Direction comes from
// classification (income, or else expense), never the amount's sign, which
// Sure inverts for inflows; an amount is amount_cents when present, else
// amount (a number, or text such as "$1,234.56"). Amounts are exact, in
// two decimal places.
func sureMonthly(doc any, p Params) (any, error) {
	records, err := harness.Records("the source", doc, p.Items)
	if err != nil {
		return nil, err
	}
	type acc struct {
		Month
		income, spending int64
		cats             map[string]*catAcc
	}
	months := map[string]*acc{}
	for i, r := range records {
		date, _ := harness.Lookup(r, "date")
		ds, ok := date.(string)
		if !ok || len(ds) < 7 || ds[4] != '-' {
			return nil, fmt.Errorf("transaction %d: date %v is not YYYY-MM-DD", i, date)
		}
		key := ds[:7]
		m := months[key]
		if m == nil {
			m = &acc{Month: Month{Month: key}, cats: map[string]*catAcc{}}
			months[key] = m
		}
		if isTransfer(r) {
			m.Transfers++
			continue
		}
		cents, err := amountCents(r)
		if err != nil {
			return nil, fmt.Errorf("transaction %d: %w", i, err)
		}
		if cents < 0 {
			cents = -cents
		}
		name := Uncategorized
		if v, ok := harness.Lookup(r, "category.name"); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				name = s
			}
		}
		c := m.cats[name]
		if c == nil {
			c = &catAcc{name: name}
			m.cats[name] = c
		}
		c.count++
		m.Transactions++
		if cls, _ := harness.Lookup(r, "classification"); cls == "income" {
			m.income += cents
			c.income += cents
		} else {
			m.spending += cents
			c.spending += cents
		}
	}
	keys := make([]string, 0, len(months))
	for k := range months {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Month, 0, len(keys))
	for _, k := range keys {
		m := months[k]
		m.Income, m.Spending, m.Net = dollars(m.income), dollars(m.spending), dollars(m.income-m.spending)
		cats := make([]*catAcc, 0, len(m.cats))
		for _, c := range m.cats {
			cats = append(cats, c)
		}
		sort.Slice(cats, func(i, j int) bool {
			if cats[i].spending != cats[j].spending {
				return cats[i].spending > cats[j].spending
			}
			return cats[i].name < cats[j].name
		})
		m.Categories = make([]Category, 0, len(cats))
		for _, c := range cats {
			m.Categories = append(m.Categories, Category{Category: c.name, Spending: dollars(c.spending), Income: dollars(c.income), Count: c.count})
		}
		out = append(out, m.Month)
	}
	return map[string]any{"months": out, "transactions": len(records)}, nil
}

type catAcc struct {
	name             string
	spending, income int64
	count            int
}

// isTransfer is true for a transaction Sure paired with another account's.
func isTransfer(r any) bool {
	v, ok := harness.Lookup(r, "transfer")
	return ok && v != nil && v != false
}

// amountCents reads a transaction's amount in cents, exactly.
func amountCents(r any) (int64, error) {
	if v, ok := harness.Lookup(r, "amount_cents"); ok && v != nil {
		n, ok := v.(json.Number)
		if !ok {
			return 0, fmt.Errorf("amount_cents %v is not a number", v)
		}
		c, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("amount_cents %s is not whole cents", n)
		}
		return c, nil
	}
	v, ok := harness.Lookup(r, "amount")
	if !ok || v == nil {
		return 0, fmt.Errorf("no amount_cents or amount")
	}
	var lit string
	switch x := v.(type) {
	case json.Number:
		lit = x.String()
	case string:
		lit = strings.NewReplacer("$", "", ",", "", " ", "").Replace(x)
	default:
		return 0, fmt.Errorf("amount %v is not a number", v)
	}
	rat, ok := new(big.Rat).SetString(lit)
	if !ok {
		return 0, fmt.Errorf("amount %v is not a number", v)
	}
	rat.Mul(rat, big.NewRat(100, 1))
	if !rat.IsInt() || !rat.Num().IsInt64() {
		return 0, fmt.Errorf("amount %v is not whole cents", v)
	}
	return rat.Num().Int64(), nil
}

// dollars writes cents as an exact two-place decimal.
func dollars(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}
