package v1

import (
    "log"
    "fmt"
    "sync"
    "strconv"
    "net"
    "net/rpc"
    "net/http"
    "time"
    //"errors"
    "compress/gzip"
    "io"
    "bytes"
    "regexp"
    "io/ioutil"
    "encoding/json"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/ltkh/netmap/internal/config"
    "github.com/ltkh/netmap/internal/client"
    "github.com/ltkh/netmap/internal/db"
)

var (
    httpClient = client.NewHttpClient(nil)

    resultCode = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "netmap",
            Name:      "result_code",
            Help:      "",
        },
        []string{"src_name","dst_name","mode","port"},
    )

    responseTime = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Namespace: "netmap",
            Name:      "response_time",
            Help:      "",
        },
        []string{"src_name","dst_name","mode","port"},
    )

    connections = make(map[string]chan int)
)

type Api struct {
    Conf         *config.Config            `json:"conf"`
    Peers        *Peers                    `json:"peers"`
    DB           *db.DbClient              `json:"db"`
}

type Resp struct {
    Status       string                    `json:"status"`
    Error        string                    `json:"error,omitempty"`
    Warnings     []string                  `json:"warnings,omitempty"`
    Data         []interface{}             `json:"data"`
}

type Records struct {
    sync.RWMutex
    items        map[string]config.SockTable
}

type Exceptions struct {
    sync.RWMutex
    items        map[string]config.Exception
}

type Peers struct {
    sync.RWMutex
    items        map[string]*rpc.Client
}

type Errors struct {
    sync.RWMutex
    items        map[string]error
}

func readUserIP(r *http.Request) string {
    IPAddress := r.Header.Get("X-Real-Ip")
    if IPAddress == "" {
        IPAddress = r.Header.Get("X-Forwarded-For")
    }
    if IPAddress == "" {
        IPAddress = r.RemoteAddr
    }
    return IPAddress
}

func encodeResp(resp *Resp) []byte {
    if len(resp.Data) == 0 {
        resp.Data = make([]interface{}, 0)
    }

    jsn, err := json.Marshal(resp)
    if err != nil {
        return encodeResp(&Resp{Status:"error", Error:err.Error(), Data:make([]interface{}, 0)})
    }
    return jsn
}

func compressData(data []byte, encoding string) (bytes.Buffer, bool, error) {
    var buf bytes.Buffer
    // Send compressed data if needed
    matched, _ := regexp.MatchString(`gzip`, encoding)
    if matched {
        writer := gzip.NewWriter(&buf)
        if _, err := writer.Write(data); err != nil {
            return buf, false, fmt.Errorf("unable to compress data")
        }
        if err := writer.Close(); err != nil {
            return buf, false, fmt.Errorf("unable to compress data")
        }
        return buf, true, nil
    } 
    
    return *bytes.NewBuffer(data), false, nil
}

func MonRegister(){
    prometheus.MustRegister(resultCode)
    prometheus.MustRegister(responseTime)
}

func NewAPI(conf *config.Config, peers []string, db db.DbClient) (*Api, error) {
    api := &Api{
        Conf: conf,
        Peers: &Peers{items: make(map[string]*rpc.Client)},
        DB: &db,
    }

    for _, id := range peers {
        connections[id] = make(chan int, 1)
    }

    return api, nil
}

func (api *Api) ApiPeers() {
    for id, _ := range connections {

        conn, err := net.DialTimeout("tcp", id, 2 * time.Second)
        if err == nil {
            if _, ok := api.Peers.items[id]; !ok {
                api.Peers.Lock()
                api.Peers.items[id] = rpc.NewClient(conn)
                api.Peers.Unlock()
                log.Printf("[info] successful connection: %v", id)
                continue
            }
            if len(connections[id]) > 0 {
                <- connections[id]
                api.Peers.Lock()
                api.Peers.items[id] = rpc.NewClient(conn)
                api.Peers.Unlock()
                log.Printf("[info] connection restored: %v", id)
                continue
            }
        } else {
            log.Printf("[error] %v", err)
        }
        
    }
}

func (api *Api) ApiHealthy(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(200)
    w.Write([]byte("OK"))
}

func (api *Api) ApiStatus(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var netstat config.NetstatData

        if err := json.Unmarshal(body, &netstat); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            go func(id string, cli *rpc.Client) {
    
                err := client.Call("RPC.SetStatus", netstat.Data, nil)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
            }(id, client)
            
        }
        
        w.WriteHeader(204)
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}

func (api *Api) ApiNetstat(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var netstat config.NetstatData

        if err := json.Unmarshal(body, &netstat); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            go func(id string, client *rpc.Client) {
    
                err := client.Call("RPC.SetNetstat", netstat.Data, nil)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
            }(id, client)
            
        }
        
        w.WriteHeader(204)
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}

func (api *Api) ApiTracert(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var netstat config.NetstatData

        if err := json.Unmarshal(body, &netstat); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            go func(id string, client *rpc.Client) {
    
                err := client.Call("RPC.SetTracert", netstat.Data, nil)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
            }(id, client)
            
        }
        
        w.WriteHeader(204)
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}

func (api *Api) ApiRecords(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    var wg sync.WaitGroup
    var records []config.SockTable

    if r.Method == "GET" {

        //rc := Records{items: make(map[string]config.SockTable)}
        var args config.RecArgs

        for k, v := range r.URL.Query() {
            switch k {
                case "id":
                    args.Id = v[0]
                case "type":
                    args.Type = v[0]
                case "src_name":
                    args.SrcName = v[0]
                case "timestamp":
                    i, err := strconv.Atoi(v[0])
                    if err != nil {
                        w.WriteHeader(400)
                        w.Write(encodeResp(&Resp{Status:"error", Error:fmt.Sprintf("executing query: invalid parameter: %v", k)}))
                        return
                    }
                    args.Timestamp = int64(i)
            }
        }

        items, err := db.DbClient.LoadRecords(*api.DB, args)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var records []interface{}
        for _, item := range items{
            if item.Timestamp < args.Timestamp {
                continue
            }
            records = append(records, item)
        }

        data := encodeResp(&Resp{Status:"success", Data:records})
        /*
        buf, ok, err := compressData(data, r.Header.Get("Accept-Encoding"))
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        if ok {
            w.Header().Set("Content-Encoding", "gzip")
        }
        */

        w.WriteHeader(200)
        w.Write(data)
        return

        /*
        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items { 

            wg.Add(1)

            go func(id string, client *rpc.Client, rc *Records, wg *sync.WaitGroup) {
                defer wg.Done()
    
                var items []config.SockTable
                err := client.Call("RPC.GetRecords", args, &items)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
                rc.Lock()
                defer rc.Unlock()
    
                for _, item := range items{
                    if args.Timestamp > item.Timestamp {
                        continue
                    }
                    if it, ok := rc.items[item.Id]; ok {
                        if it.Timestamp >= item.Timestamp {
                            continue
                        }
                    }
                    rc.items[item.Id] = item
                }
    
            }(id, client, &rc, &wg)
            
        }

        wg.Wait()

        rc.RLock()
        defer rc.RUnlock()

        for _, item := range rc.items{
            records = append(records, item)
        }

        if len(records) == 0 {
            records = make([]config.SockTable, 0)
        }

        data := encodeResp(&Resp{Status:"success", Data:records})
        buf, ok, err := compressData(data, r.Header.Get("Accept-Encoding"))
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        if ok {
            w.Header().Set("Content-Encoding", "gzip")
        }

        w.WriteHeader(200)
        w.Write(buf.Bytes())
        return
        */
    }

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var netstat config.NetstatData

        if err := json.Unmarshal(body, &netstat); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        rhost := readUserIP(r)

        for _, nr := range netstat.Data {
            if nr.LocalAddr.Name == "" {
                log.Printf("[error] parameter missing localAddr.name, sender - %s", rhost)
                continue
            }
            if nr.LocalAddr.IP == nil {
                log.Printf("[error] parameter missing LocalAddr.IP, sender - %s", rhost)
                continue
            }
            if nr.RemoteAddr.Name == "" {
                log.Printf("[error] parameter missing RemoteAddr.Name, sender - %s", rhost)
                continue
            }
            if nr.RemoteAddr.IP == nil {
                log.Printf("[error] parameter missing RemoteAddr.IP, sender - %s", rhost)
                continue
            }
            if nr.Relation.Port == 0 {
                log.Printf("[error] parameter missing Relation.Port, sender - %s", rhost)
                continue
            }
            if nr.Relation.Mode == "" {
                log.Printf("[error] parameter missing Relation.Mode, sender - %s", rhost)
                continue
            }
            //nr.Id = config.GetIdRec(&nr)
            records = append(records, nr)
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            wg.Add(1)

            go func(id string, client *rpc.Client) {
                defer wg.Done()

                err := client.Call("RPC.SetRecords", records, nil)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }

            }(id, client)
            
        }

        wg.Wait()
        
        w.WriteHeader(204)
        return
    }

    if r.Method == "DELETE" {

        er := Errors{items: make(map[string]error)}
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var keys []string

        if err := json.Unmarshal(body, &keys); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            wg.Add(1)

            go func(id string, client *rpc.Client, er *Errors) {
                defer wg.Done()
    
                err := client.Call("RPC.DelRecords", keys, nil)
                if err != nil {
                    er.Lock()
                    er.items[id] = err
                    er.Unlock()

                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                }
    
            }(id, client, &er)
            
        }

        wg.Wait()

        er.RLock()
        defer er.RUnlock()

        for _, err := range er.items {
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        w.WriteHeader(200)
        w.Write(encodeResp(&Resp{Status:"success"}))
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}

func (api *Api) ApiExceptions(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    var wg sync.WaitGroup
    var exceptions []interface{}

    if r.Method == "GET" {

        ex := Exceptions{items: make(map[string]config.Exception)}
        var args config.ExpArgs

        for k, v := range r.URL.Query() {
            switch k {
                case "id":
                    args.Id = v[0]
                case "src_name":
                    args.SrcName = v[0]
                case "account_id":
                    args.AccountID = v[0]
            }
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            wg.Add(1)

            go func(id string, client *rpc.Client) {
                defer wg.Done()
    
                var items []config.Exception
                err := client.Call("RPC.GetExceptions", args, &items)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
                ex.Lock()
                defer ex.Unlock()
    
                for _, item := range items{
                    ex.items[item.Id] = item
                }
    
            }(id, client)
            
        }

        wg.Wait()

        ex.RLock()
        defer ex.RUnlock()

        for _, item := range ex.items{
            exceptions = append(exceptions, item)
        }

        data := encodeResp(&Resp{Status:"success", Data:exceptions})
        buf, ok, err := compressData(data, r.Header.Get("Accept-Encoding"))
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }
        if ok {
            w.Header().Set("Content-Encoding", "gzip")
        }

        w.WriteHeader(200)
        w.Write(buf.Bytes())
        return
    }

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var expdata config.ExceptionData

        if err := json.Unmarshal(body, &expdata); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        for _, ex := range expdata.Data {
            if ex.Id == "" {
                ex.Id = config.GetIdExp(&ex)
            } 
            exceptions = append(exceptions, ex)
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            go func(id string, client *rpc.Client) {
    
                err := client.Call("RPC.SetExceptions", exceptions, nil)
                if err != nil {
                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                    return
                }
    
            }(id, client)
            
        }
        
        w.WriteHeader(204)
        return
    }

    if r.Method == "DELETE" {

        er := Errors{items: make(map[string]error)}
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        var keys []string

        if err := json.Unmarshal(body, &keys); err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        api.Peers.RLock()
        defer api.Peers.RUnlock()
        for id, client := range api.Peers.items {

            wg.Add(1)

            go func(id string, client *rpc.Client, er *Errors) {
                defer wg.Done()
    
                err := client.Call("RPC.DelExceptions", keys, nil)
                if err != nil {
                    er.Lock()
                    er.items[id] = err
                    er.Unlock()

                    log.Printf("[error] %v - %s%s", err, id, r.URL.Path)
                    if len(connections[id]) < 1 {
                        connections[id] <- 1
                    }
                }
    
            }(id, client, &er)
            
        }
        
        wg.Wait()

        er.RLock()
        defer er.RUnlock()

        for _, err := range er.items {
            w.WriteHeader(500)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        w.WriteHeader(200)
        w.Write(encodeResp(&Resp{Status:"success"}))
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}

func (api *Api) ApiWebhook(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    if r.Method == "POST" {
        var reader io.ReadCloser
        var err error

        // Check that the server actual sent compressed data
        switch r.Header.Get("Content-Encoding") {
            case "gzip":
                reader, err = gzip.NewReader(r.Body)
                if err != nil {
                    log.Printf("[error] %v - %s", err, r.URL.Path)
                    w.WriteHeader(400)
                    w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
                    return
                }
                defer reader.Close()
            default:
                reader = r.Body
        }
        defer r.Body.Close()

        body, err := ioutil.ReadAll(reader)
        if err != nil {
            log.Printf("[error] %v - %s", err, r.URL.Path)
            w.WriteHeader(400)
            w.Write(encodeResp(&Resp{Status:"error", Error:err.Error()}))
            return
        }

        if len(api.Conf.Notifier.URLs) > 0 {
            for _, url := range api.Conf.Notifier.URLs {
                config := client.HttpConfig{
                    URLs: []string{url},
                }
                if api.Conf.Notifier.Path == "" {
                    api.Conf.Notifier.Path = "/api/v1/alerts"
                }
                go httpClient.WriteRecords(config, api.Conf.Notifier.Path, body)
            }
        }
        
        w.WriteHeader(204)
        return
    }

    w.WriteHeader(405)
    w.Write(encodeResp(&Resp{Status:"error", Error:"method not allowed"}))
}