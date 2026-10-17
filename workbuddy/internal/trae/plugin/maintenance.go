package plugin

import (
	"time"
)

func (e *Engine) maintenance() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	lastHour := ""
	lastCheckin := ""
	nextCheckin := time.Time{}
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			cfg := e.settings()
			local := now.In(zone)
			hour := local.Format("2006-01-02-15")
			day := local.Format("2006-01-02")
			checkin := cfg.CheckinAuto && local.Format("15:04") >= cfg.CheckinTime && lastCheckin != day && !now.Before(nextCheckin)
			refresh := cfg.TokenKeepalive && lastHour != hour
			if !checkin && !refresh {
				continue
			}
			files, err := e.listAuths()
			if err != nil {
				continue
			}
			allCheckinsSucceeded := true
			for _, f := range files {
				if e.ctx.Err() != nil {
					return
				}
				if unavailable(f) {
					continue
				}
				physical, err := e.getAuth(f.AuthIndex)
				if err != nil {
					allCheckinsSucceeded = false
					continue
				}
				a, _, err := e.ensureAuth(f.AuthIndex, physical.JSON, false, "", "")
				if err != nil {
					allCheckinsSucceeded = false
					continue
				}
				if checkin {
					client := e.clientFactory("", "")
					checked, _, enabled, err := client.CheckinStatus(a)
					if err == nil && enabled && !checked {
						err = client.CheckinClaim(a)
					}
					if err != nil {
						allCheckinsSucceeded = false
					}
				}
			}
			if refresh {
				lastHour = hour
			}
			if checkin && allCheckinsSucceeded {
				lastCheckin = day
			} else if checkin {
				nextCheckin = now.Add(15 * time.Minute)
			}
		}
	}
}
