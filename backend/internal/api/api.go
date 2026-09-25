// Package api exposes the REST interface. All handlers read from the canonical
// in-memory market state; none of them mutate it.
//
// Prices and quantities are fixed-point integers (see /api/meta for scales) so
// the client can apply binary deltas without floating-point drift.
package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"cryptofeed/internal/config"
	"cryptofeed/internal/market"
	"cryptofeed/internal/model"
	"cryptofeed/internal/protocol"
	"cryptofeed/internal/ws"
)

type API struct {
	cfg     config.Config
	market  *market.Market
	ws      *ws.Server
	started time.Time
}

func New(cfg config.Config, m *market.Market, s *ws.Server) *API {
	return &API{cfg: cfg, market: m, ws: s, started: time.Now()}
}

func (a *API) Register(r *gin.Engine) {
	r.Use(a.cors())
	g := r.Group("/api", accessLog(), compress())
	g.GET("/health", a.health)
	g.GET("/meta", a.meta)
	g.GET("/ticker", a.ticker)
	g.GET("/candles", a.candles)
	g.GET("/orderbook/snapshot", a.orderbook)
	g.GET("/trades", a.trades)
	g.GET("/ws/status", a.wsStatus)
	g.POST("/debug/clients/:id/tier", a.debugTier)
}

func (a *API) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		for _, o := range a.cfg.AllowedOrigins {
			if o == "*" || o == origin {
				c.Header("Access-Control-Allow-Origin", orDefault(origin, "*"))
				c.Header("Vary", "Origin")
				c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Content-Type")
				break
			}
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type apiError struct {
	Error string `json:"error"`
}

func bad(c *gin.Context, msg string) { c.JSON(http.StatusBadRequest, apiError{msg}) }

func (a *API) checkSymbol(c *gin.Context) bool {
	if s := c.Query("symbol"); s != "" && !strings.EqualFold(s, a.market.Symbol()) {
		c.JSON(http.StatusNotFound, apiError{"unknown symbol " + s})
		return false
	}
	return true
}

func intParam(c *gin.Context, key string, def, lo, hi int64) (int64, bool) {
	v := c.Query(key)
	if v == "" {
		return def, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		bad(c, key+" must be an integer in ["+strconv.FormatInt(lo, 10)+", "+strconv.FormatInt(hi, 10)+"]")
		return 0, false
	}
	return n, true
}

func (a *API) health(c *gin.Context) {
	t, d := a.market.Counts()
	c.JSON(http.StatusOK, gin.H{
		"status": "ok", "symbol": a.market.Symbol(), "uptimeSec": int(time.Since(a.started).Seconds()),
		"tradesProcessed": t, "depthUpdates": d, "clients": len(a.ws.Status()),
	})
}

// meta describes numeric scales and protocol constants.
func (a *API) meta(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"symbol": a.market.Symbol(), "priceScale": model.PriceScale, "qtyScale": model.QtyScale,
		"tickSize": a.market.TickSize(), "intervals": []string{"1m", "5m"}, "wsPath": a.cfg.WSPath,
		"packets": gin.H{
			"CHART_DELTA": protocol.ChartDeltaSize, "DEPTH_DELTA": protocol.DepthDeltaSize,
			"TRADE_UPDATE": protocol.TradeUpdateSize, "PING": protocol.PingSize, "PONG": protocol.PingSize,
		},
	})
}

func (a *API) ticker(c *gin.Context) {
	if !a.checkSymbol(c) {
		return
	}
	t := a.market.Ticker()
	c.JSON(http.StatusOK, gin.H{
		"symbol": a.market.Symbol(), "ltp": t.LTP, "ltq": t.LTQ, "open24h": t.Open24h,
		"high24h": t.High24h, "low24h": t.Low24h, "volume24h": t.Volume24h,
	})
}

type candlesResponse struct {
	Symbol   string         `json:"symbol"`
	Interval string         `json:"interval"`
	Seq      uint32         `json:"seq"`
	LTP      int64          `json:"ltp"`
	Candles  []model.Candle `json:"candles"`
	Active   *model.Candle  `json:"active"`
}

// candles returns closed history plus the active candle at canonical chart seq.
// The seq is the base the client SYNCs from before applying CHART_DELTA packets.
func (a *API) candles(c *gin.Context) {
	if !a.checkSymbol(c) {
		return
	}
	interval := c.DefaultQuery("interval", "1m")
	limit, ok := intParam(c, "limit", 200, 1, 5000)
	if !ok {
		return
	}
	v, err := a.market.Candles(interval, int(limit))
	if errors.Is(err, market.ErrUnknownInterval) {
		bad(c, "interval must be one of 1m, 5m")
		return
	}
	if v.Candles == nil {
		v.Candles = []model.Candle{}
	}
	c.JSON(http.StatusOK, candlesResponse{a.market.Symbol(), interval, v.Seq, v.LTP, v.Candles, v.Active})
}

type bookResponse struct {
	Symbol   string        `json:"symbol"`
	Seq      uint32        `json:"seq"`
	LTP      int64         `json:"ltp"`
	LTQ      int64         `json:"ltq"`
	TickSize int64         `json:"tickSize"`
	Bids     []model.Level `json:"bids"`
	Asks     []model.Level `json:"asks"`
}

func (a *API) orderbook(c *gin.Context) {
	if !a.checkSymbol(c) {
		return
	}
	v := a.market.BookSnapshot()
	c.JSON(http.StatusOK, bookResponse{
		a.market.Symbol(), v.Book.Seq, v.LTP, v.LTQ, a.market.TickSize(), v.Book.Bids[:], v.Book.Asks[:],
	})
}

// trades serves time-range history from the bounded ring buffer, newest first.
func (a *API) trades(c *gin.Context) {
	if !a.checkSymbol(c) {
		return
	}
	from, ok := intParam(c, "from", 0, 0, 1<<53)
	if !ok {
		return
	}
	to, ok := intParam(c, "to", 0, 0, 1<<53)
	if !ok {
		return
	}
	limit, ok := intParam(c, "limit", 200, 1, 5000)
	if !ok {
		return
	}
	if to > 0 && from > to {
		bad(c, "from must be <= to")
		return
	}
	list, oldest := a.market.TradeRange(from, to, int(limit))
	if list == nil {
		list = []model.Trade{}
	}
	c.JSON(http.StatusOK, gin.H{"symbol": a.market.Symbol(), "trades": list, "retainedFrom": oldest})
}

func (a *API) wsStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"clients": a.ws.Status()})
}

// debugTier forces a connection's tier (FULL/DEGRADED/MINIMAL) or returns it to AUTO.
// Demonstration/debug only; the same control exists over the WebSocket.
func (a *API) debugTier(c *gin.Context) {
	var body struct {
		Tier string `json:"tier"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		bad(c, "body must be {\"tier\": \"AUTO|FULL|DEGRADED|MINIMAL\"}")
		return
	}
	switch err := a.ws.SetOverrideByID(c.Param("id"), strings.ToUpper(body.Tier)); {
	case errors.Is(err, ws.ErrNoClient):
		c.JSON(http.StatusNotFound, apiError{err.Error()})
	case err != nil:
		bad(c, err.Error())
	default:
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "override": strings.ToUpper(body.Tier)})
	}
}
