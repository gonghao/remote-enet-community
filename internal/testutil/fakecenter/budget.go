package fakecenter

// Limits uses fixed windows on the fixture clock. Zero selects contract defaults.
// All loopback entries represent the same source IP.
type Limits struct {
	LoginGlobal, LoginIP, LoginPrefix    int
	RedeemGlobal, RedeemIP, RedeemDevice int
	CreatePerAccount, RedeemPerAccount   int
}
type counterKey struct{ kind, principal string }
type counter struct {
	until int64
	count int
}
type budget struct {
	key     counterKey
	limit   int
	seconds int64
}

func limit(kind, principal string, configured, standard int, seconds int64) budget {
	if configured == 0 {
		configured = standard
	}
	return budget{counterKey{kind, principal}, configured, seconds}
}
func (c *Center) remaining(b budget, now int64) int64 {
	v := c.counters[b.key]
	if now < v.until && v.count >= b.limit {
		return v.until - now
	}
	return 0
}
func (c *Center) count(b budget, now int64) {
	v := c.counters[b.key]
	if now >= v.until {
		v = counter{until: now + b.seconds}
	}
	v.count++
	c.counters[b.key] = v
}
func (c *Center) withBudget(a *call) result {
	l := c.opts.Limits
	var limits []budget
	switch a.op {
	case "login":
		limits = []budget{limit("login-global", "", l.LoginGlobal, 10, 60), limit("login-ip", "loopback", l.LoginIP, 10, 600), limit("login-prefix", a.loginInput.AccountCode[:2], l.LoginPrefix, 20, 3600)}
	case "redeem":
		limits = []budget{limit("redeem-global", "", l.RedeemGlobal, 60, 60), limit("redeem-ip", "loopback", l.RedeemIP, 10, 600), limit("redeem-device", a.fp, l.RedeemDevice, 10, 600)}
	case "create":
		limits = []budget{limit("create", a.d.account.id, l.CreatePerAccount, 30, 3600)}
	default:
		return c.execute(a)
	}
	for _, b := range limits {
		if wait := c.remaining(b, a.now); wait > 0 {
			return waitError("rate_limited", wait)
		}
	}
	if a.op == "redeem" {
		for _, b := range limits {
			c.count(b, a.now)
		}
		if s := c.codes[a.redeemInput.Code]; s != nil {
			b := limit("redeemed", s.account.id, l.RedeemPerAccount, 30, 3600)
			if c.remaining(b, a.now) > 0 {
				c.end(s, "redeem_limit", a.now)
				return failure("code_invalid")
			}
			c.count(b, a.now)
		}
	}
	r := c.execute(a)
	if a.op == "login" && r.code != "" || a.op == "create" && r.code == "" {
		for _, b := range limits {
			c.count(b, a.now)
		}
	}
	return r
}
