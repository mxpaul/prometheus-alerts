package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/valyala/fasthttp"
)

type CmdLineOptions struct {
	MetricsQuery   PrometheusQuery
	TokenFilePath  string
	TelegramChatID int64
	AlertThreshold int
}

func ParseCommandLineOptionsOrDie() *CmdLineOptions {
	opt := CmdLineOptions{}
	flag.StringVar(&opt.MetricsQuery.Host, "prometheus-url",
		"http://localhost:9090/",
		"prometheus server address with scheme, address and port",
	)
	flag.StringVar(&opt.MetricsQuery.Query, "query",
		"wbx_catalog_storage_limit-wbx_catalog_storage_size",
		"metric name used as query API param",
	)
	flag.DurationVar(&opt.MetricsQuery.RequestTimeout, "request-timeout",
		2*time.Second,
		"request timeout for prometheus API",
	)
	flag.StringVar(&opt.TokenFilePath, "telegram-bot-token-file",
		"~/.secret/telegram.bot.token",
		"path to file with telegram bot token (talk to @BotFather to get one)",
	)
	flag.Int64Var(&opt.TelegramChatID, "telegram-chat-id",
		int64(0),
		"telegram chat id to report alerts to",
	)
	flag.IntVar(&opt.AlertThreshold, "alert-threshold",
		int(1000),
		"send alert if there is at leas one shard with FreeProductSlots less then this value",
	)
	flag.Parse()

	if opt.TelegramChatID <= 0 {
		log.Fatalf("telegram-chat-id is required, add @RawDataBot to your chat to find it out")
	}

	return &opt
}

type Scope struct {
	HttpClient     *fasthttp.Client
	TelegramBot    *tgbotapi.BotAPI
	TelegramChatID int64
}

func NewScope(opt *CmdLineOptions) (*Scope, error) {
	scope := &Scope{
		HttpClient:     &fasthttp.Client{MaxConnsPerHost: 10},
		TelegramChatID: opt.TelegramChatID,
	}

	botToken, err := BotTokenFromFile(opt)
	if err != nil {
		return nil, fmt.Errorf("telegram token read error: %w", err)
	}
	err = scope.CreateTelegramBot(botToken)
	if err != nil {
		return nil, fmt.Errorf("CreateTelegramBot error: %w", err)
	}

	return scope, nil
}

func (scope *Scope) CreateTelegramBot(botToken string) (err error) {
	scope.TelegramBot, err = tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("bot api init error: %w", err)
	}
	return nil
}

func (scope *Scope) SendLimitsAlert(alertShards CategoryShardStatusList) error {
	msg := alertShards.MarkdownMessage(scope.TelegramChatID)
	if _, err := scope.TelegramBot.Send(msg); err != nil {
		return fmt.Errorf("TelegramBot.Send error: %w", err)
	}
	return nil
}

type PrometheusQuery struct {
	Host           string
	RequestTimeout time.Duration
	Query          string
}

func (param *PrometheusQuery) DoRequest(scope *Scope) ([]byte, error) {
	request := fasthttp.AcquireRequest()
	response := fasthttp.AcquireResponse()
	defer func() {
		fasthttp.ReleaseResponse(response)
		fasthttp.ReleaseRequest(request)
	}()

	var urlReq []byte
	urlReq = append(urlReq, param.Host...)
	urlReq = append(urlReq, `/api/v1/query?query=`...)
	urlReq = append(urlReq, param.Query...)
	request.SetRequestURIBytes(urlReq)
	request.Header.SetMethod("GET")
	//request.Header.Add("Some-Header", param.ClientID)
	if err := scope.HttpClient.DoTimeout(request, response, param.RequestTimeout); err != nil {
		log.Println("Request error:", err.Error())
		return []byte(""), err
	}
	if response.StatusCode() != fasthttp.StatusOK {
		return []byte(""), fmt.Errorf("req %q status code: %d", urlReq, response.StatusCode())
	}
	//if h := string(response.Header.Peek("Content-Type")); h != "application/octet-stream" {
	//	return []byte(""), fmt.Errorf("req %q content type invalid: %s", urlReq, h)
	//}
	//if h := string(response.Header.Peek("Content-Encoding")); h != "gzip" {
	//	return []byte(""), fmt.Errorf("req %q content encoding invalid: %s", urlReq, h)
	//}
	return response.Body(), nil
}

type PrometheusResponse struct {
	Data      PrometheusData `json:"data"`
	Status    string         `json:"status"`
	ErrorType string         `json:"errorType"`
	Error     string         `json:"error"`
	Warnings  []string       `json:"warnings"`
}

type PrometheusData struct {
	ResultType string
	Result     []PrometheusResult
}

type PrometheusResult struct {
	Metric PrometheusResultMetric `json:"metric"`
	Value  []interface{}          `json:"value"`
}

type PrometheusResultMetric struct {
	Name      string `json:"__name__"`
	Shard     string `json:"shard"`
	ShardType string `json:"shard_type"`
}

type CategoryShardStatusList []*CategoryShardStatus

func (states CategoryShardStatusList) String() string {
	body := make([]byte, 0, 256)
	body = append(body, "["...)
	for i, state := range states {
		if i > 0 {
			body = append(body, ","...)
		}
		body = append(body, state.Shard...)
	}
	body = append(body, "]"...)
	return string(body)
}

func (states CategoryShardStatusList) MarkdownMessage(chatID int64) tgbotapi.Chattable {
	shardTitle := "shard"
	countTitle := "left"
	shardMaxLen := len(shardTitle)
	countMaxLen := len(countTitle)
	for _, state := range states {
		if len(state.Shard) > shardMaxLen {
			shardMaxLen = len(state.Shard)
		}
	}

	body := make([]byte, 0, 256)
	body = append(body, "**Shard limit alert**\n\n```\n"...)

	body = addSeparator(body, shardMaxLen, countMaxLen)
	body = addHeader(body, shardTitle, shardMaxLen, countTitle, countMaxLen)
	body = addSeparator(body, shardMaxLen, countMaxLen)

	stateFormat := fmt.Sprintf("| %%-%ds | %%-%dd |\n", shardMaxLen, countMaxLen)
	for _, state := range states {
		body = append(body, fmt.Sprintf(stateFormat, state.Shard, state.FreeProductSlots)...)
	}
	body = addSeparator(body, shardMaxLen, countMaxLen)
	body = append(body, "```\n"...)

	msg := tgbotapi.NewMessage(chatID, string(body))
	msg.ParseMode = "markdown"

	return msg
}

func repeatString(body []byte, pattern string, count int) []byte {
	for i := 0; i < count; i++ {
		body = append(body, pattern...)
	}
	return body
}

func addHeader(body []byte, shardTitle string, shardMaxLen int, countTitle string, countMaxLen int) []byte {
	body = append(body, "| "...)
	body = append(body, shardTitle...)
	body = repeatString(body, " ", shardMaxLen-len(shardTitle)+1)
	body = append(body, "| "...)
	body = append(body, countTitle...)
	body = repeatString(body, " ", countMaxLen-len(countTitle)+1)
	body = append(body, "|\n"...)
	return body
}
func addSeparator(body []byte, shardMaxLen, countMaxLen int) []byte {
	body = append(body, "| "...)
	body = repeatString(body, "-", shardMaxLen)
	body = append(body, " | "...)
	body = repeatString(body, "-", countMaxLen)
	body = append(body, " |\n"...)
	return body
}

type CategoryShardStatus struct {
	Shard            string
	FreeProductSlots int
	Time             time.Time
}

func NewCategoryShardStatusList(scope *Scope, metricsQuery *PrometheusQuery) ([]*CategoryShardStatus, error) {
	body, err := metricsQuery.DoRequest(scope)
	if err != nil {
		return nil, fmt.Errorf("prometheus/query request error: %v", err)
	}

	resp := &PrometheusResponse{}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return nil, fmt.Errorf("response JSON unmarshal: %v", err)
	}
	if resp.Status == "error" {
		return nil, fmt.Errorf("prometheus response error: type: %s; msg: %s", resp.ErrorType, resp.Error)
	}

	shardStateMap := map[string]*CategoryShardStatus{}
	for _, result := range resp.Data.Result {
		if result.Metric.ShardType != "category" {
			continue
		}
		state := &CategoryShardStatus{Shard: result.Metric.Shard}

		var intValue int
		switch value := result.Value[1].(type) {
		case string:
			intValue, err = strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("invalid value for shard %s: %v", state.Shard, err)
			}
			state.FreeProductSlots = intValue
		default:
			return nil, fmt.Errorf("value type invalid %T", value)
		}

		switch value := result.Value[0].(type) {
		case float64:
			sec, dec := math.Modf(value)
			state.Time = time.Unix(int64(sec), int64(dec*(1e9)))
		default:
			return nil, fmt.Errorf("time type invalid %T", value)
		}

		if existingState, found := shardStateMap[state.Shard]; found {
			if existingState.FreeProductSlots > state.FreeProductSlots {
				shardStateMap[state.Shard] = state
			}
		} else {
			shardStateMap[state.Shard] = state
		}
	}

	shardStates := make([]*CategoryShardStatus, 0, len(shardStateMap))
	for _, state := range shardStateMap {
		shardStates = append(shardStates, state)
	}

	return shardStates, nil
}

func BotTokenFromFile(opt *CmdLineOptions) (string, error) {
	path := opt.TokenFilePath
	if strings.HasPrefix(path, "~/") {
		curUser, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("user.Current() error: %v", err)
		}
		path = filepath.Join(curUser.HomeDir, path[2:])
	}

	if !filepath.IsAbs(path) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("Failed to make absolute path from %q: %v", path, err)
		}
		path = absPath
	}

	slackTokenBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("Failed to read gitlab token from file %q: %v", path, err)
	}
	slackToken := strings.TrimSuffix(string(slackTokenBytes), "\n")
	return slackToken, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt := ParseCommandLineOptionsOrDie()
	log.Printf("OPTS %+v", opt)

	scope, err := NewScope(opt)
	if err != nil {
		log.Fatalf("scope create error: %v", err)
	}

	reqStartTile := time.Now()
	log.Printf("send prometheus request to %s", opt.MetricsQuery.Host)
	shardStatusList, err := NewCategoryShardStatusList(scope, &opt.MetricsQuery)
	log.Printf("request complete in %v", time.Since(reqStartTile))

	if err != nil {
		log.Fatalf("NewCategoryShardStatusList error: %v", err)
	}

	alertShards := CategoryShardStatusList{}
	for _, state := range shardStatusList {
		if state.FreeProductSlots > opt.AlertThreshold {
			continue
		}
		alertShards = append(alertShards, state)
	}

	if len(alertShards) == 0 {
		log.Printf("no shard require limit increase")
		return
	}

	sort.Slice(alertShards, func(i, j int) bool {
		return alertShards[j].FreeProductSlots > alertShards[i].FreeProductSlots
	})

	log.Printf("shard limits alert for %s", alertShards.String())

	err = scope.SendLimitsAlert(alertShards)
	if err != nil {
		log.Fatalf("alert send telegram error: %v", err)
	}
}
