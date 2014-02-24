package main

import (
    "log"
    "fmt"
    "net/http"
    "strconv"
    "bytes"
    "encoding/json"
    "strings"
    "time"
    "math"
    "io"
    "io/ioutil"
    "html/template"

    "github.com/codegangsta/martini"
    "github.com/codegangsta/martini-contrib/render"
    "github.com/codegangsta/martini-contrib/binding"
    "github.com/pmylund/go-cache"
    "github.com/garyburd/redigo/redis"
    "github.com/grafov/bcast"

    "btcplex"
)

// Martini form for the search input
type searchForm struct {
    Query string `form:"q"`
}

// Struct holding page meta data, like meta tags, and some template variables
type pageMeta struct {
    Title string
    Description string
    Menu string
    Block *btcplex.Block
    Blocks *[]*btcplex.Block
    Tx *btcplex.Tx
    TxUnconfirmed bool
    Txs *[]*btcplex.Tx
    AddressData *btcplex.AddressData
    LastHeight uint
    CurrentHeight uint
    Error string
    Price float64
    paginationData *paginationData
}

type paginationData struct {
    CurrentPage int
    MaxPage int
    Next int
    Prev int
    Pages []struct{}
}

type RedisWrapper struct {
    Pool *redis.Pool
}

const (
    ratelimitwindow = 3600
    ratelimitcnt = 3600
)

// Used to rate-limit the API
func rateLimited(rediswrapper *RedisWrapper, ip string) (bool, int, int) {
    conn := rediswrapper.Pool.Get()
    defer conn.Close()
    reset := int(time.Now().UTC().Unix() / ratelimitwindow * ratelimitwindow + ratelimitwindow)
    ipkey := fmt.Sprintf("rl:%v:%v", ip, reset)
    cnt, _ := redis.Int(conn.Do("GET", ipkey))
    if cnt > ratelimitcnt {
        return true, cnt, reset
    } else {
        conn.Send("MULTI")
        conn.Send("INCR", ipkey)
        conn.Send("EXPIREAT", ipkey, reset + ratelimitwindow)
        conn.Do("EXEC")
        cnt+=1
        return false, cnt, reset
    }
}

func bcastToRedisPubSub(pool *redis.Pool, psgroup *bcast.Group, redischannel string) {
    conn := pool.Get()
    defer conn.Close()
    psc := redis.PubSubConn{Conn: conn}
    psc.Subscribe(redischannel)
    for {
        switch v := psc.Receive().(type) {
        case redis.Message:
            h1 := psgroup.Join()
            h1.Send(string(v.Data))
            h1.Close()
        }
    }
}

func addHATEOAS(links map[string]map[string]string, key string, link string) map[string]map[string]string {
    newlink := map[string]string{}
    newlink["href"] = link
    links[key] = newlink
    return links
}

func initHATEOAS(links map[string]map[string]string, req *http.Request) map[string]map[string]string {
    links = map[string]map[string]string{}
    return addHATEOAS(links, "self", req.URL.String())
}

func N(n int) []struct{} {
    return make([]struct{}, n)
}

func main() {
    log.Println("Starting btcplex-server")
    conf, conferr:= btcplex.LoadConfig("./config.json")
    if conferr != nil {
        log.Fatalf("Can't load config file: %v", conferr)
    }

    var latestheight int
    
    c := cache.New(120*time.Minute, 30*time.Second)

    // Used for pub/sub in the webapp and data like latest processed height
    pool, _ := btcplex.GetRedis(conf)
    // Due to args injection I can't use two *redis.Pool with maritini
    rediswrapper := new(RedisWrapper)
    rediswrapper.Pool = pool
    
    ssdb, _ := btcplex.GetSSDB(conf)

    // Setup some pubsub:

    // Compute the unconfirmed transaction count in a ticker
    utxscnt := 0
    utxscntticker := time.NewTicker(500 * time.Millisecond)
    go func(pool *redis.Pool, utxscnt *int) {
        c := pool.Get()
        defer c.Close()
        for _ = range utxscntticker.C {
            *utxscnt, _ = redis.Int(c.Do("ZCARD", "btcplex:rawmempool"))
        }
    }(pool, &utxscnt)

    price, _ := btcplex.GetLastBitcoinPrice()
    // PubSub channel for the latest price
    pricegroup := bcast.NewGroup()
    go pricegroup.Broadcasting(0)

    ticker := time.NewTicker(5 * time.Minute)
    go func(pricegroup *bcast.Group) {
        for _ = range ticker.C {
            cprice, priceerr := btcplex.GetLastBitcoinPrice()
            if priceerr == nil {
                price = cprice
                log.Printf("New price: %v\n", cprice)
                price1 := pricegroup.Join()
                price1.Send(fmt.Sprintf("{\"t\": \"price\", \"price\": %v}", cprice))
                price1.Close()
            }
        }
    }(pricegroup)

    // PubSub channel for the current height
    heightgroup := bcast.NewGroup()
    go heightgroup.Broadcasting(0)
    go bcastToRedisPubSub(pool, heightgroup, "btcplex:height")

    // PubSub channel for blocknotify bitcoind RPC like
    blocknotifygroup := bcast.NewGroup()
    go blocknotifygroup.Broadcasting(0)
    go bcastToRedisPubSub(pool, blocknotifygroup, "btcplex:blocknotify")
    
    // PubSub channel for unconfirmed txs / rawmemorypool
    utxgroup := bcast.NewGroup()
    go utxgroup.Broadcasting(0)
    go bcastToRedisPubSub(pool, utxgroup, "btcplex:utxs")
    // TODO Ticker for utxs count => events_unconfirmed

    // Go template helper
    appHelpers := template.FuncMap{
        "cut": func(addr string, length int) string {
            return fmt.Sprintf("%v...", addr[:length])
        },
        "cutmiddle": func(addr string, length int) string {
            return fmt.Sprintf("%v...%v", addr[:length], addr[len(addr) - length:])
        },
        "tokb": func(size uint32) string {
            return fmt.Sprintf("%.3f", float32(size) / 1024)
        },
        "computefee": func(tx *btcplex.Tx) string {
            if tx.TotalIn == 0 {
                return "0"
            }
            return fmt.Sprintf("%v", float32(tx.TotalIn - tx.TotalOut) / 1e8)
        },
        "generationmsg": func(tx *btcplex.Tx) string {
            reward := btcplex.GetBlockReward(tx.BlockHeight)
            fee := float64(tx.TotalOut - uint64(reward)) / 1e8
            return fmt.Sprintf("%v BTC + %.8f total fees", float64(reward) / 1e8, fee)
        },
        "tobtc": func(val uint64) string {
            return fmt.Sprintf("%.8f", float64(val) / 1e8)
        },
        "inttobtc": func(val int64) string {
            return fmt.Sprintf("%.8f", float64(val) / 1e8)
        },
        "formatprevout": func(prevout *btcplex.PrevOut) string {
            return fmt.Sprintf("%v:%v", prevout.Hash, prevout.Vout)
        },
        "formattime": func(ts uint32) string {
            return fmt.Sprintf("%v", time.Unix(int64(ts), 0).UTC())
        },
        "formatiso": func(ts uint32) string {
            return fmt.Sprintf("%v", time.Unix(int64(ts), 0).Format(time.RFC3339))
        },
        "sub": func(h, p uint) uint {
            return h - p
        },
        "add": func(h, p uint) uint {
            return h + p
        },
        "iadd": func(h, p int) int {
            return h + p
        },
        "confirmation": func(height uint) uint {
            return uint(latestheight) - height + 1
        },

    }

    conn := ssdb.Get()    
    latestheight, _ = redis.Int(conn.Do("GET", "height:latest"))
    log.Printf("Latest height: %v\n", latestheight)
    latesthash := ""
    conn.Close()

    m := martini.Classic()
    m.Map(c)
    m.Map(rediswrapper)
    m.Map(ssdb)

    tmpldir := "templates"
    if conf.AppTemplatesPath != "" {
        tmpldir = conf.AppTemplatesPath
    }
    m.Use(render.Renderer(render.Options{
        Directory: tmpldir,
        Layout: "layout",
        Funcs: []template.FuncMap{appHelpers},
    }))

    // We rate limit the API if enabled in the config
    if conf.AppApiRateLimited {
        m.Use(func(res http.ResponseWriter, req *http.Request, rediswrapper *RedisWrapper, log *log.Logger) {
            remoteIP := strings.Split(req.RemoteAddr,":")[0]
            _, xforwardedfor := req.Header["X-Forwarded-For"]
            if xforwardedfor {
                remoteIP = req.Header["X-Forwarded-For"][0]
            }
            log.Printf("R:%v\nip:%+v\n", time.Now(), remoteIP)
            if strings.Contains(req.RequestURI, "/api/v") {
                ratelimited, cnt, reset := rateLimited(rediswrapper, remoteIP)
                // Set X-RateLimit-* Header
                res.Header().Set("X-RateLimit-Limit", strconv.Itoa(ratelimitcnt))
                res.Header().Set("X-RateLimit-Remaining", strconv.Itoa(ratelimitcnt - cnt))
                res.Header().Set("X-RateLimit-Reset", strconv.Itoa(reset))
                // Set CORS header
                res.Header().Set("Access-Control-Expose-Headers", " X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset")
                res.Header().Set("Access-Control-Allow-Origin", "*")
                
                if ratelimited {
                    res.WriteHeader(429)
                }
            }
        })
    }

    // Don't want Google to crawl API
    m.Get("/robots.txt", func() string {
        return "User-agent: *\nDisallow: /api"
    })

    m.Get("/", func(r render.Render, c *cache.Cache, db *redis.Pool) {
        pm := new(pageMeta)
        pm.Price = price
        blocks, _ := btcplex.GetLastXBlocks(db, uint(latestheight), uint(latestheight - 30))
        pm.Blocks = &blocks
        pm.Title = "Latest Bitcoin blocks"
        pm.Description = "Open source Bitcoin block chain explorer with JSON API"
        pm.Menu = "latest_blocks"
        pm.LastHeight = uint(latestheight)
        r.HTML(200, "index", &pm)
    })

    m.Get("/blocks/:currentheight", func(params martini.Params, r render.Render, db *redis.Pool) {
        pm := new(pageMeta)
        pm.Price = price
        currentheight, _ := strconv.ParseUint(params["currentheight"], 10, 0)
        blocks, _ := btcplex.GetLastXBlocks(db, uint(currentheight), uint(currentheight - 30))
        pm.Blocks = &blocks
        pm.Title = "Bitcoin blocks"
        pm.Menu = "blocks"
        pm.LastHeight = uint(latestheight)
        pm.CurrentHeight = uint(currentheight)
        r.HTML(200, "blocks", &pm)
    })

    m.Get("/block/:hash", func(params martini.Params, r render.Render, db *redis.Pool) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        block, _ := btcplex.GetBlockCachedByHash(db, params["hash"])
        //block.FetchTxs(db)
        pm.Block = block
        pm.Title = fmt.Sprintf("Bitcoin block #%v", block.Height)
        pm.Description = fmt.Sprintf("Bitcoin block #%v summary and related transactions", block.Height)
        r.HTML(200, "block", &pm)
    })

    m.Get("/api/v1/block/:hash", func(params martini.Params, r render.Render, db *redis.Pool, req *http.Request) {
        block, _ := btcplex.GetBlockCachedByHash(db, params["hash"])
        //block.FetchTxs(db)
        block.Links = initHATEOAS(block.Links, req)
        if block.Parent != "" {
            block.Links = addHATEOAS(block.Links, "previous_block", fmt.Sprintf("/api/v1/block/%v", block.Parent))    
        }
        if block.Next != "" {
            block.Links = addHATEOAS(block.Links, "next_block", fmt.Sprintf("/api/v1/block/%v", block.Next))    
        }
        r.JSON(200, block)
    })
    
    m.Get("/unconfirmed-transactions", func(params martini.Params, r render.Render, db *redis.Pool, rdb *RedisWrapper) {
        //rpool := rdb.Pool
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Menu = "utxs"
        pm.Title = "Unconfirmed transactions"
        pm.Description = "Transactions waiting to be included in a Bitcoin block, updated in real time."
        //utxs, _ := btcplex.GetUnconfirmedTxs(rpool)
        pm.Txs = &[]*btcplex.Tx{}
        r.HTML(200, "unconfirmed-transactions", &pm)
    })

    m.Get("/tx/:hash", func(params martini.Params, r render.Render, db *redis.Pool, rdb *RedisWrapper) {
        var tx *btcplex.Tx
        rpool := rdb.Pool
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        isutx, _ := btcplex.IsUnconfirmedTx(rpool, params["hash"])
        if isutx {
            pm.TxUnconfirmed = true
            tx, _ = btcplex.GetUnconfirmedTx(rpool, params["hash"])
        } else {
            tx, _ = btcplex.GetTx(db, params["hash"])
            tx.Build(db)
        }
        pm.Tx = tx
        pm.Title = fmt.Sprintf("Bitcoin transaction %v", tx.Hash)
        pm.Description = fmt.Sprintf("Bitcoin transaction %v summary.", tx.Hash)
        r.HTML(200, "tx", pm)
    })
    m.Get("/api/v1/tx/:hash", func(params martini.Params, r render.Render, db *redis.Pool, rdb *RedisWrapper, req *http.Request) {
        var tx *btcplex.Tx
        rpool := rdb.Pool
        isutx, _ := btcplex.IsUnconfirmedTx(rpool, params["hash"])
        if isutx {
            tx, _ = btcplex.GetUnconfirmedTx(rpool, params["hash"])
        } else {
            tx, _ = btcplex.GetTx(db, params["hash"])
            tx.Build(db)
        }
        tx.Links = initHATEOAS(tx.Links, req)
        if tx.BlockHash != "" {
            tx.Links = addHATEOAS(tx.Links, "block", fmt.Sprintf("/api/v1/block/%v", tx.BlockHash))
        }
        r.JSON(200, tx)
    })

    m.Get("/address/:address", func(params martini.Params, r render.Render, db *redis.Pool, req *http.Request) {
        txperpage := 50
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.paginationData = new(paginationData)
        pm.Title = fmt.Sprintf("Bitcoin address %v", params["address"])
        pm.Description = fmt.Sprintf("Transactions and summary for the Bitcoin address %v.", params["address"])
        // AddressData
        addressdata, _ := btcplex.GetAddress(db, params["address"])
        pm.AddressData = addressdata
        // Pagination
        d := float64(addressdata.TxCnt) / float64(txperpage)
        pm.paginationData.MaxPage = int(math.Ceil(d))
        currentPage := req.URL.Query().Get("page")
        if currentPage == "" {
            currentPage = "1"
        }
        pm.paginationData.CurrentPage, _ = strconv.Atoi(currentPage)
        pm.paginationData.Pages = N(pm.paginationData.MaxPage)
        pm.paginationData.Next = 0
        pm.paginationData.Prev = 0
        if pm.paginationData.CurrentPage > 1 {
            pm.paginationData.Prev = pm.paginationData.CurrentPage - 1
        }
        if pm.paginationData.CurrentPage < pm.paginationData.MaxPage {
            pm.paginationData.Next = pm.paginationData.CurrentPage + 1
        }
        fmt.Printf("%+v\n", pm.paginationData)
        // Fetch txs given the pagination
        addressdata.FetchTxs(db, txperpage * (pm.paginationData.CurrentPage - 1), txperpage * pm.paginationData.CurrentPage)
        r.HTML(200, "address", pm)
    })
    m.Get("/api/v1/address/:address", func(params martini.Params, r render.Render, db *redis.Pool, req *http.Request) {
        txperpage := 50
        addressdata, _ := btcplex.GetAddress(db, params["address"])
        lastPage := int(math.Ceil(float64(addressdata.TxCnt) / float64(txperpage)))
        currentPageStr := req.URL.Query().Get("page")
        if currentPageStr == "" {
            currentPageStr = "1"
        }
        currentPage, _ := strconv.Atoi(currentPageStr)
        // HATEOS section
        addressdata.Links = initHATEOAS(addressdata.Links, req)
        pageurl := "/api/v1/address/%v?page=%v"
        if currentPage < lastPage {
            addressdata.Links = addHATEOAS(addressdata.Links, "last", fmt.Sprintf(pageurl, params["address"], lastPage))
            addressdata.Links = addHATEOAS(addressdata.Links, "next", fmt.Sprintf(pageurl, params["address"], currentPage + 1))
        }
        if currentPage > 1 {
            addressdata.Links = addHATEOAS(addressdata.Links, "previous", fmt.Sprintf(pageurl, params["address"], currentPage - 1))
        }
        addressdata.FetchTxs(db, txperpage * (currentPage - 1), txperpage * currentPage)
        r.JSON(200, addressdata)
    })

    m.Get("/docs/api", func(r render.Render) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Title = "API Documentation"
        pm.Description = "BTCPlex provides JSON API for developers to retrieve Bitcoin block chain data pragmatically"
        pm.Menu = "api"
        r.HTML(200, "api_docs", pm)
    })

    m.Get("/docs/query_api", func(r render.Render) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Title = "Query API Documentation"
        pm.Description = "BTCPlex provides JSON API for developers to retrieve Bitcoin block chain data pragmatically"
        pm.Menu = "api"
        // TODO menu2
        r.HTML(200, "docs_query_api", pm)
    })

    m.Get("/docs/rest_api", func(r render.Render) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Title = "REST API Documentation"
        pm.Description = "BTCPlex provides JSON API for developers to retrieve Bitcoin block chain data pragmatically"
        pm.Menu = "api"
        // TODO menu2
        r.HTML(200, "docs_rest_api", pm)
    })

    m.Get("/docs/sse_api", func(r render.Render) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Title = "Server-Sent Events API Documentation"
        pm.Description = "BTCPlex provides JSON API for developers to retrieve Bitcoin block chain data pragmatically"
        pm.Menu = "api"
        // TODO menu2
        r.HTML(200, "docs_sse_api", pm)
    })

    m.Get("/about", func(r render.Render) {
        pm := new(pageMeta)
        pm.Price = price
        pm.LastHeight = uint(latestheight)
        pm.Title = "About"
        pm.Description = "Learn more about BTCPlex, an open source Bitcoin block chain explorer with JSON API"
        pm.Menu = "about"
        r.HTML(200, "about", pm)
    })

    m.Post("/search", binding.Form(searchForm{}), binding.ErrorHandler, func(search searchForm, r render.Render, db *redis.Pool, rdb *RedisWrapper) {
        rpool := rdb.Pool
        pm := new(pageMeta)
        pm.Price = price
        // Check if the query isa block height
        isblockheight, hash := btcplex.IsBlockHeight(db, search.Query)
        if isblockheight && hash != "" {
            r.Redirect(fmt.Sprintf("/block/%v", hash))
        }
        // Check if the query is block hash
        isblockhash, hash := btcplex.IsBlockHash(db, search.Query)
        if isblockhash {
            r.Redirect(fmt.Sprintf("/block/%v", hash))    
        }
        // Check for TX
        istxhash, txhash := btcplex.IsTxHash(db, search.Query)
        if istxhash {
            r.Redirect(fmt.Sprintf("/tx/%v", txhash))
        }
        isutx, txhash := btcplex.IsUnconfirmedTx(rpool, search.Query)
        if isutx {
            r.Redirect(fmt.Sprintf("/tx/%v", txhash))
        }
        // Check for Bitcoin address
        isaddress, address := btcplex.IsAddress(search.Query)
        if isaddress {
            r.Redirect(fmt.Sprintf("/address/%v", address))
        }
        pm.Title = "Search"
        pm.Error = "Nothing found"
        r.HTML(200, "search", pm)
    })

    m.Get("/api/v1/getblockcount", func(r render.Render) {
        r.JSON(200, latestheight)
    })

    m.Get("/api/v1/latesthash", func(r render.Render) {
        r.JSON(200, latesthash)
    })

    m.Get("/api/v1/getblockhash/:height", func(r render.Render, params martini.Params, db *redis.Pool) {
        height, _ := strconv.ParseUint(params["height"], 10, 0)
        blockhash, _ := btcplex.GetBlockHash(db, uint(height))
        r.JSON(200, blockhash)
    })

    m.Get("/api/v1/getreceivedbyaddress/:address", func(r render.Render, params martini.Params, db *redis.Pool) {
        res, _ := btcplex.GetReceivedByAddress(db, params["address"])
        r.JSON(200, res)
    })

    m.Get("/api/v1/getsentbyaddress/:address", func(r render.Render, params martini.Params, db *redis.Pool) {
        res, _ := btcplex.GetSentByAddress(db, params["address"])
        r.JSON(200, res)
    })

    m.Get("/api/v1/addressbalance/:address", func(r render.Render, params martini.Params, db *redis.Pool) {
        res, _ := btcplex.AddressBalance(db, params["address"])
        r.JSON(200, res)
    })

    m.Get("/api/v1/checkaddress/:address", func(params martini.Params, r render.Render) {
        valid, _ := btcplex.ValidA58([]byte(params["address"]))
        r.JSON(200, valid)
    })

    m.Get("/api/v1/blocknotify", func(w http.ResponseWriter, r *http.Request) {
        running := true
        notifier := w.(http.CloseNotifier).CloseNotify()
        timer := time.NewTimer(time.Second *1300)

        f, _ := w.(http.Flusher)
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")

        bnotifier := blocknotifygroup.Join()
        defer bnotifier.Close()

        var ls interface{}
        for {
            if running {
                select {
                    case ls = <-bnotifier.In:
                        io.WriteString(w, fmt.Sprintf("data: %v\n\n", ls.(string)))
                        f.Flush()
                    case <-notifier:
                        running = false
                        log.Println("CLOSED")
                        break
                    case <-timer.C:
                        running = false
                        log.Println("TimeOUT")
                }
            } else {
                log.Println("DONE")
                break
            }
        }
    })

    m.Get("/events", func(w http.ResponseWriter, r *http.Request) {
        running := true
        notifier := w.(http.CloseNotifier).CloseNotify()
        timer := time.NewTimer(time.Second *900)

        f, _ := w.(http.Flusher)
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")

        p2 := pricegroup.Join()
        defer p2.Close()
        h2 := heightgroup.Join()
        defer h2.Close()

        var ls interface{}
        for {
            if running {
                select {
                    case ls = <-p2.In:
                        io.WriteString(w, fmt.Sprintf("data: %v\n\n", ls.(string)))
                        f.Flush()
                    case ls = <-h2.In:
                        io.WriteString(w, fmt.Sprintf("data: %v\n\n", ls.(string)))
                        f.Flush()
                    case <-notifier:
                        running = false
                        log.Println("CLOSED")
                        break
                    case <-timer.C:
                        running = false
                        log.Println("TimeOUT")
                }
            } else {
                log.Println("DONE")
                break
            }
        }
    })

    m.Get("/events_unconfirmed", func(w http.ResponseWriter, r *http.Request) {
        running := true
        notifier := w.(http.CloseNotifier).CloseNotify()
        timer := time.NewTimer(time.Second * 900)

        f, _ := w.(http.Flusher)
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")

        utx := utxgroup.Join()
        defer utx.Close()

        var ls interface{}
        for {
            if running {
                select {
                    case ls = <-utx.In:
                        buf := bytes.NewBufferString("")
                        utx := new(btcplex.Tx)
                        json.Unmarshal([]byte(ls.(string)), utx)
                        t := template.New("").Funcs(appHelpers)
                        utxtmpl, _ := ioutil.ReadFile(fmt.Sprintf("%v/utx.tmpl", tmpldir))
                        t, err := t.Parse(string(utxtmpl))
                        if err != nil {
                            log.Printf("ERR:%v", err)
                        }
                        
                        err = t.Execute(buf, utx)
                        if err != nil {
                            log.Printf("ERR EXEC:%v", err)
                        }
                        res := map[string]interface{}{}
                        // Full unconfirmed cnt from global variables
                        res["cnt"] = utxscnt
                        // HTML template of the transaction
                        res["tmpl"] = buf.String()
                        // Last updated time
                        res["time"] = time.Now().UTC().Format(time.RFC3339)
                        resjson, _ := json.Marshal(res)
                        io.WriteString(w, fmt.Sprintf("data: %v\n\n", string(resjson)))
                        f.Flush()
                    case <-notifier:
                        running = false
                        log.Println("CLOSED")
                        break
                    case <-timer.C:
                        running = false
                        log.Println("TimeOUT")
                }
            } else {
                log.Println("DONE")
                break
            }
        }
    })

    log.Printf("Listening on port: %v\n", conf.AppPort)
    http.ListenAndServe(fmt.Sprintf(":%v", conf.AppPort), m)
}