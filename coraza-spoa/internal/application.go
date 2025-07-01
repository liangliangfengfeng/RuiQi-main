package internal

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"

	flowcontroller "github.com/HUAHUAI23/RuiQi/coraza-spoa/internal/flow-controller"
	"github.com/HUAHUAI23/RuiQi/pkg/model"
	coreruleset "github.com/corazawaf/coraza-coreruleset"
	"github.com/corazawaf/coraza/v3"
	"github.com/corazawaf/coraza/v3/debuglog"
	"github.com/corazawaf/coraza/v3/types"
	"github.com/dropmorepackets/haproxy-go/pkg/encoding"
	"github.com/jcchavezs/mergefs"
	"github.com/jcchavezs/mergefs/io"
	"github.com/rs/zerolog"
	"istio.io/istio/pkg/cache"
)

// MongoDB 配置
type MongoConfig struct {
	Client     *mongo.Client
	Database   string
	Collection string
}

type AppConfig struct {
	Directives     string
	ResponseCheck  bool
	Logger         zerolog.Logger
	TransactionTTL time.Duration
}

// ApplicationOptions 应用程序配置选项 配置应用是否开启 ip 解析，日志记录
type ApplicationOptions struct {
	MongoConfig          *MongoConfig          // MongoDB配置，用于日志存储
	GeoIPConfig          *GeoIP2Options        // GeoIP配置，用于IP地理位置处理
	RuleEngineDbConfig   *MongoDBConfig        // 规则引擎数据库配置
	FlowControllerConfig *FlowControllerConfig // 流量控制器配置
}

// FlowControllerConfig 流量控制器配置
type FlowControllerConfig struct {
	Client   *mongo.Client // MongoDB客户端
	Database string        // 数据库名称
}

type Application struct {
	waf            coraza.WAF
	cache          cache.ExpiringCache
	logStore       LogStore
	ipProcessor    IPProcessor
	ruleEngine     *RuleEngine
	flowController *flowcontroller.FlowController
	ipRecorder     flowcontroller.IPRecorder

	AppConfig
}

// 扩展transaction结构体，添加请求信息
type transaction struct {
	tx      types.Transaction
	m       sync.Mutex
	request *applicationRequest // 存储请求信息
}

type applicationRequest struct {
	SrcIp   netip.Addr
	SrcPort int64
	DstIp   netip.Addr
	DstPort int64
	Method  string
	ID      string
	Path    []byte
	Query   []byte
	Version string
	Headers []byte
	Body    []byte
}

func (a *Application) HandleRequest(ctx context.Context, writer *encoding.ActionWriter, message *encoding.Message) (err error) {
	k := encoding.AcquireKVEntry()
	// run defer via anonymous function to not directly evaluate the arguments.
	defer func() {
		encoding.ReleaseKVEntry(k)
	}()

	// parse request
	var req applicationRequest
	for message.KV.Next(k) {
		switch name := string(k.NameBytes()); name {
		case "src-ip":
			req.SrcIp = k.ValueAddr()
		case "src-port":
			req.SrcPort = k.ValueInt()
		case "dst-ip":
			req.DstIp = k.ValueAddr()
		case "dst-port":
			req.DstPort = k.ValueInt()
		case "method":
			req.Method = string(k.ValueBytes())
		case "path":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			req.Path = currK.ValueBytes()

			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		case "query":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			req.Query = currK.ValueBytes()
			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		case "version":
			req.Version = string(k.ValueBytes())
		case "headers":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			req.Headers = currK.ValueBytes()
			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		case "body":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			req.Body = currK.ValueBytes()
			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		case "id":
			req.ID = string(k.ValueBytes())
		default:
			a.Logger.Debug().Str("name", name).Msg("unknown kv entry")
		}
	}

	if len(req.ID) == 0 {
		const idLength = 16
		var sb strings.Builder
		sb.Grow(idLength)
		for i := 0; i < idLength; i++ {
			sb.WriteRune(rune('A' + rand.Intn(26)))
		}
		req.ID = sb.String()
	}

	realIP := getRealClientIP(&req)
	// 检查IP是否已被限制
	if a.ipRecorder != nil {
		if blocked, record := a.ipRecorder.IsIPBlocked(realIP); blocked {
			a.Logger.Info().
				Str("ip", realIP).
				Str("reason", record.Reason).
				Time("blocked_until", record.BlockedUntil).
				Msg("请求被拒绝：IP已被限制")

			return ErrInterrupted{
				Interruption: &types.Interruption{
					Action: "deny",
					Status: 403,
					Data:   fmt.Sprintf("IP has been blocked until %s due to %s", record.BlockedUntil.Format(time.RFC3339), record.Reason),
				},
			}
		}
	}

	host := getHostFromRequest(&req)
	// 进行高频访问检查
	if a.flowController != nil {
		allowed, err := a.flowController.CheckVisit(realIP, buildFullURL(host, req.Path, req.Query))
		if err != nil {
			a.Logger.Error().Err(err).Str("ip", realIP).Msg("流控检查失败")
		} else if !allowed {
			return ErrInterrupted{
				Interruption: &types.Interruption{
					Action: "deny",
					Status: 429,
					Data:   "Too many requests",
				},
			}
		}
	}

	// micro engine detection
	if a.ruleEngine != nil {
		realIP := getRealClientIP(&req)
		// 获取路径部分
		path := string(req.Path)

		url := buildURLFromBytes(req.Path, req.Query)

		shouldBlock, _, rule, err := a.ruleEngine.MatchRequest(realIP, url, path)

		if err != nil {
			a.Logger.Error().Err(err).
				Str("url", url).
				Str("clientIP", realIP).
				Msg("failed to match request")
		}

		ruleName := "whitelist block"
		ruleId := "none"
		if rule != nil {
			ruleName = rule.Name
			ruleId = rule.ID.String()
		}

		if shouldBlock && err == nil {
			// 记录攻击
			if a.flowController != nil {
				_, _ = a.flowController.RecordAttack(realIP, buildFullURL(host, req.Path, req.Query))
			}

			a.Logger.Info().
				Str("ruleName", ruleName).
				Str("ruleId", ruleId).
				Str("url", url).
				Str("clientIP", realIP).
				Msg("request blocked by micro engine")

			err := a.saveMicroEngineLog(rule, &req, req.Headers)
			if err != nil {
				a.Logger.Error().Err(err).
					Str("ruleName", ruleName).
					Str("ruleId", ruleId).
					Str("url", url).
					Str("clientIP", realIP).
					Msg("failed to save micro engine log")
			}

			return ErrInterrupted{
				Interruption: &types.Interruption{
					Action: "deny",
					Status: 403,
				},
			}
		}
	}

	tx := a.waf.NewTransactionWithID(req.ID)
	defer func() {
		if err == nil && a.ResponseCheck {
			// 存储transaction和请求信息到缓存
			txCache := &transaction{
				tx:      tx,
				request: &req, // 存储请求信息
			}
			a.cache.SetWithExpiration(tx.ID(), txCache, a.TransactionTTL)
			return
		}

		// 处理中断情况和日志记录
		if tx.IsInterrupted() && a.logStore != nil {
			// 记录攻击
			if a.flowController != nil {
				_, _ = a.flowController.RecordAttack(realIP, buildFullURL(host, req.Path, req.Query))
			}

			interruption := tx.Interruption()
			if matchedRules := tx.MatchedRules(); len(matchedRules) > 0 {
				err := a.saveFirewallLog(matchedRules, interruption, &req, req.Headers)
				if err != nil {
					a.Logger.Error().Err(err).Msg("failed to save firewall log")
				}
			}
		}

		tx.ProcessLogging()
		if err := tx.Close(); err != nil {
			a.Logger.Error().Str("tx", tx.ID()).Err(err).Msg("failed to close transaction")
		}
	}()

	// 设置 response id 为事务 id，为 response 检测提供支持
	if err := writer.SetString(encoding.VarScopeTransaction, "id", tx.ID()); err != nil {
		return err
	}

	if tx.IsRuleEngineOff() {
		a.Logger.Warn().Msg("Rule engine is Off, Coraza is not going to process any rule")
		return nil
	}

	tx.ProcessConnection(req.SrcIp.String(), int(req.SrcPort), req.DstIp.String(), int(req.DstPort))

	tx.ProcessURI(buildURLFromBytes(req.Path, req.Query), req.Method, "HTTP/"+req.Version)

	if err := readHeaders(req.Headers, tx.AddRequestHeader); err != nil {
		return fmt.Errorf("reading headers: %v", err)
	}

	if it := tx.ProcessRequestHeaders(); it != nil {
		return ErrInterrupted{it}
	}

	switch it, _, err := tx.WriteRequestBody(req.Body); {
	case err != nil:
		return err
	case it != nil:
		return ErrInterrupted{it}
	}

	switch it, err := tx.ProcessRequestBody(); {
	case err != nil:
		return err
	case it != nil:
		return ErrInterrupted{it}
	}

	return nil
}

// readHeaders parses HTTP headers with optimized performance while maintaining correctness
func readHeaders(headers []byte, callback func(key string, value string)) error {
	if len(headers) == 0 {
		return nil
	}

	start := 0
	length := len(headers)

	for start < length {
		// 查找行尾
		end := start
		for end < length && headers[end] != '\n' {
			end++
		}

		// 获取当前行
		line := headers[start:end]

		// 处理 \r\n (去除 \r)
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// 跳过空行
		if len(line) == 0 {
			start = end + 1
			continue
		}

		// 查找冒号
		colonPos := -1
		for i, b := range line {
			if b == ':' {
				colonPos = i
				break
			}
		}

		if colonPos == -1 {
			return fmt.Errorf("invalid header: %q", string(line))
		}

		// 提取并trim key和value
		keyBytes := line[:colonPos]
		valueBytes := line[colonPos+1:]

		keyStart, keyEnd := trimSpaceIndices(keyBytes)
		valueStart, valueEnd := trimSpaceIndices(valueBytes)

		// 检查key是否为空（与原始版本保持一致的行为）
		if keyStart >= keyEnd {
			// 对于空key，原始版本的bytes.SplitN不会报错，而是创建空字符串
			// 我们也保持这种行为
			key := ""
			value := string(valueBytes[valueStart:valueEnd])
			callback(key, value)
		} else {
			key := string(keyBytes[keyStart:keyEnd])
			value := string(valueBytes[valueStart:valueEnd])
			callback(key, value)
		}

		// 移动到下一行
		start = end + 1
	}

	return nil
}

// trimSpaceIndices 返回去除前后空白字符后的起始和结束索引
// 避免内存分配，只返回索引
func trimSpaceIndices(data []byte) (start, end int) {
	// 跳过前导空白
	start = 0
	for start < len(data) && isWhitespace(data[start]) {
		start++
	}

	// 跳过尾随空白
	end = len(data)
	for end > start && isWhitespace(data[end-1]) {
		end--
	}

	return start, end
}

// isWhitespace 检查字符是否为空白字符（与bytes.TrimSpace行为一致）
func isWhitespace(b byte) bool {
	// 包含所有bytes.TrimSpace处理的空白字符
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

type applicationResponse struct {
	ID      string
	Version string
	Status  int64
	Headers []byte
	Body    []byte
}

func (a *Application) HandleResponse(ctx context.Context, writer *encoding.ActionWriter, message *encoding.Message) (err error) {
	if !a.ResponseCheck {
		return fmt.Errorf("got response but response check is disabled")
	}

	k := encoding.AcquireKVEntry()
	// run defer via anonymous function to not directly evaluate the arguments.
	defer func() {
		encoding.ReleaseKVEntry(k)
	}()

	var res applicationResponse
	for message.KV.Next(k) {
		switch name := string(k.NameBytes()); name {
		case "id":
			res.ID = string(k.ValueBytes())
		case "version":
			res.Version = string(k.ValueBytes())
		case "status":
			res.Status = k.ValueInt()
		case "headers":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			res.Headers = currK.ValueBytes()
			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		case "body":
			// make a copy of the pointer and add a defer in case there is another entry
			currK := k
			// run defer via anonymous function to not directly evaluate the arguments.
			defer func() {
				encoding.ReleaseKVEntry(currK)
			}()

			res.Body = currK.ValueBytes()
			// acquire a new kv entry to continue reading other message values.
			k = encoding.AcquireKVEntry()
		default:
			a.Logger.Debug().Str("name", name).Msg("unknown kv entry")
		}
	}

	if res.ID == "" {
		return fmt.Errorf("response id is empty")
	}

	cv, ok := a.cache.Get(res.ID)
	if !ok {
		a.Logger.Error().Str("id", res.ID).Msg("transaction not found")
		return nil
		// TODO: 是否需要报错，还是仅记录，检测器重启时这里会报错，因为 application 被替换，a.cache.Get 会拿不到 res.ID 对应的 transaction
		// return fmt.Errorf("transaction not found: %s", res.ID)
	}
	a.cache.Remove(res.ID)

	t := cv.(*transaction)
	if !t.m.TryLock() {
		return fmt.Errorf("transaction is already being deleted: %s", res.ID)
	}
	/*
		确实不需要 defer t.m.Unlock()，因为能够走到 TryLock 就说明 a.cache.Remove(res.ID) 一定被执行，
		tx 一定被删除，TryLock 失败有两种情况，一种是 cache 回收拿到了，此时 tx 被回收了，
		另一种就是 其他 go 程拿到了，那么没拿到就直接结束，让其他拿到的 go 程处理，这样就保证了 response 只被处理一次
	*/
	tx := t.tx

	// 获取真实客户端IP
	realIP := getRealClientIP(t.request)
	host := getHostFromRequest(t.request)
	if res.Status >= 400 {
		// 检查错误响应并记录
		// 记录错误
		if a.flowController != nil {
			_, _ = a.flowController.RecordError(realIP, buildFullURL(host, t.request.Path, t.request.Query))
		}
	}

	defer func() {
		// 处理中断情况和日志记录
		if tx.IsInterrupted() && a.logStore != nil {
			// 记录攻击
			if a.flowController != nil {
				_, _ = a.flowController.RecordAttack(realIP, buildFullURL(host, t.request.Path, t.request.Query))
			}

			interruption := tx.Interruption()
			if matchedRules := tx.MatchedRules(); len(matchedRules) > 0 && t.request != nil {
				err := a.saveFirewallLog(matchedRules, interruption, t.request, t.request.Headers)
				if err != nil {
					a.Logger.Error().Err(err).Msg("failed to save firewall log")
				}
			}
		}

		tx.ProcessLogging()
		if err := tx.Close(); err != nil {
			a.Logger.Error().Str("tx", tx.ID()).Err(err).Msg("failed to close transaction")
		}
	}()

	if tx.IsRuleEngineOff() {
		goto exit
	}

	if err := readHeaders(res.Headers, tx.AddResponseHeader); err != nil {
		return fmt.Errorf("reading headers: %v", err)
	}

	if it := tx.ProcessResponseHeaders(int(res.Status), "HTTP/"+res.Version); it != nil {
		return ErrInterrupted{it}
	}

	switch it, _, err := tx.WriteResponseBody(res.Body); {
	case err != nil:
		return err
	case it != nil:
		return ErrInterrupted{it}
	}

	switch it, err := tx.ProcessResponseBody(); {
	case err != nil:
		return err
	case it != nil:
		return ErrInterrupted{it}
	}

exit:
	return nil
}

// 构建HTTP请求字符串
func buildRequestString(req *applicationRequest, headers []byte) string {
	// 预计算总容量
	capacity := len(req.Method) + 1 + // Method + space
		len(req.Path) + // Path
		len(req.Version) + 6 + // " HTTP/" + version
		1 + // \n
		len(headers) // headers

	if req.Query != nil {
		capacity += 1 + len(req.Query) // ? + query
	}

	if len(req.Body) > 0 {
		capacity += 1 + len(req.Body) // \n + body
	}

	// 使用预计算的容量初始化 Builder
	var sb strings.Builder
	sb.Grow(capacity)

	// 构建请求字符串
	sb.WriteString(req.Method)
	sb.WriteByte(' ')
	sb.Write(req.Path)
	if req.Query != nil {
		sb.WriteByte('?')
		sb.Write(req.Query)
	}
	sb.WriteString(" HTTP/")
	sb.WriteString(req.Version)
	sb.WriteByte('\n')
	sb.Write(headers)

	if len(req.Body) > 0 {
		sb.WriteByte('\n')
		sb.Write(req.Body)
	}

	return sb.String()
}

func (a *Application) saveMicroEngineLog(rule *Rule, req *applicationRequest, headers []byte) error {
	// 定义常量，避免重复字符串
	const defaultRuleName = "whitelist block"
	const defaultRuleID = "none"
	const blockMessage = "request blocked by micro engine"

	// 获取客户端真实IP
	realIP := getRealClientIP(req)

	// 确定规则信息
	ruleName := defaultRuleName
	ruleID := defaultRuleID
	if rule != nil {
		ruleName = rule.Name
		ruleID = rule.ID.String()
	}

	// 构建日志消息 - 使用fmt.Sprintf而不是多次字符串拼接
	logMessage := fmt.Sprintf("%s, ruleId: %s, ruleName: %s", blockMessage, ruleID, ruleName)

	// 直接创建具有单个元素的日志切片
	logs := []model.Log{
		{
			Message: logMessage,
			LogRaw:  logMessage,
		},
	}

	now := time.Now()
	// 初始化防火墙日志
	firewallLog := model.WAFLog{
		CreatedAt:    now,
		Request:      buildRequestString(req, headers),
		Response:     "", // 暂时不处理响应
		Domain:       getHostFromRequest(req),
		SrcIP:        realIP,
		DstIP:        req.DstIp.String(),
		SrcPort:      int(req.SrcPort),
		DstPort:      int(req.DstPort),
		RequestID:    req.ID,
		Logs:         logs, // 直接在初始化时设置日志
		Payload:      logMessage,
		Date:         now.Format("2006-01-02"),
		Hour:         now.Hour(),
		HourGroupSix: now.Hour() / 6,
		Minute:       now.Minute(),
	}

	// 获取并添加源IP的地理位置信息
	if a.ipProcessor != nil && realIP != "" {
		srcIPInfo := a.ipProcessor.GetIPInfo(realIP)
		if srcIPInfo != nil {
			firewallLog.SrcIPInfo = srcIPInfo
		}
	}

	// 使用日志存储器异步存储
	return a.logStore.Store(firewallLog)
}

func (a *Application) saveFirewallLog(matchedRules []types.MatchedRule, interruption *types.Interruption, req *applicationRequest, headers []byte) error {
	// 构建日志条目
	logs := make([]model.Log, 0)

	realIP := getRealClientIP(req)
	now := time.Now()

	// 初始化防火墙日志
	firewallLog := model.WAFLog{
		CreatedAt:    now,
		Request:      buildRequestString(req, headers),
		Response:     "", // 暂时不处理响应
		Domain:       getHostFromRequest(req),
		SrcIP:        realIP,
		DstIP:        req.DstIp.String(),
		SrcPort:      int(req.SrcPort),
		DstPort:      int(req.DstPort),
		RequestID:    req.ID,
		Date:         now.Format("2006-01-02"),
		Hour:         now.Hour(),
		HourGroupSix: now.Hour() / 6,
		Minute:       now.Minute(),
	}

	// 获取并添加源IP的地理位置信息
	if a.ipProcessor != nil && realIP != "" {
		srcIPInfo := a.ipProcessor.GetIPInfo(realIP)
		if srcIPInfo != nil {
			firewallLog.SrcIPInfo = srcIPInfo
		}
	}

	// 遍历所有匹配的规则
	for _, matchedRule := range matchedRules {
		if data := matchedRule.Data(); matchedRule.Rule().ID() == interruption.RuleID || len(data) > 0 {
			// 添加日志条目
			log := model.Log{
				Message:    matchedRule.Message(),
				Payload:    matchedRule.Data(),
				RuleID:     matchedRule.Rule().ID(),
				Severity:   int(matchedRule.Rule().Severity()),
				Phase:      int(matchedRule.Rule().Phase()),
				SecMark:    matchedRule.Rule().SecMark(),
				Accuracy:   matchedRule.Rule().Accuracy(),
				SecLangRaw: matchedRule.Rule().Raw(),
				LogRaw:     matchedRule.ErrorLog(),
			}
			logs = append(logs, log)

			// 更新防火墙日志的字段（只有当新值不为空时才覆盖）
			if id := matchedRule.Rule().ID(); id != 0 {
				firewallLog.RuleID = id
			}
			if raw := matchedRule.Rule().Raw(); raw != "" {
				firewallLog.SecLangRaw = raw
			}
			if severity := matchedRule.Rule().Severity(); severity != 0 {
				firewallLog.Severity = int(severity)
			}
			if phase := matchedRule.Rule().Phase(); phase != 0 {
				firewallLog.Phase = int(phase)
			}
			if secMark := matchedRule.Rule().SecMark(); secMark != "" {
				firewallLog.SecMark = secMark
			}
			if accuracy := matchedRule.Rule().Accuracy(); accuracy != 0 {
				firewallLog.Accuracy = accuracy
			}
			if payload := matchedRule.Data(); payload != "" {
				firewallLog.Payload = payload
			}
			if msg := matchedRule.Message(); msg != "" {
				firewallLog.Message = msg
			}
			if uri := matchedRule.URI(); uri != "" {
				firewallLog.URI = uri
			}
			if clientIP := matchedRule.ClientIPAddress(); clientIP != "" {
				firewallLog.ClientIP = clientIP
			}
			if serverIP := matchedRule.ServerIPAddress(); serverIP != "" {
				firewallLog.ServerIP = serverIP
			}
		}
	}

	// 添加收集的所有日志
	firewallLog.Logs = logs

	// 使用日志存储器异步存储
	return a.logStore.Store(firewallLog)
}

// NewApplication creates a new Application with a custom context
func (a AppConfig) NewApplicationWithContext(ctx context.Context, options ApplicationOptions, isDebug bool) (*Application, error) {
	// If no context is provided, use background context
	isDev := os.Getenv("IS_DEV") == "true"
	app := &Application{
		AppConfig: a,
	}

	if ctx == nil {
		ctx = context.Background()
	}

	if options.MongoConfig != nil && options.MongoConfig.Client != nil {
		logStore := NewMongoLogStore(
			options.MongoConfig.Client,
			options.MongoConfig.Database,
			options.MongoConfig.Collection,
			a.Logger,
		)
		logStore.Start()
		app.logStore = logStore
	}

	// 根据规则引擎数据库配置初始化规则引擎
	if options.RuleEngineDbConfig != nil && options.RuleEngineDbConfig.MongoClient != nil {
		ruleEngine := NewRuleEngine()
		ruleEngine.InitMongoConfig(options.RuleEngineDbConfig)
		ruleEngine.LoadAllFromMongoDB()
		app.ruleEngine = ruleEngine
	}

	// 根据GeoIP配置初始化IP处理器
	if options.GeoIPConfig != nil {
		processor, err := NewIPProcessor(
			ctx,
			options.GeoIPConfig.CityDBPath,
			options.GeoIPConfig.ASNDBPath,
			a.Logger,
		)
		if err != nil {
			a.Logger.Warn().Err(err).Msg("初始化IP处理器失败，将使用空实现")
			app.ipProcessor = NewNullIPProcessor()
		} else {
			app.ipProcessor = processor
		}
	} else {
		// 如果未提供GeoIP配置，使用空实现
		app.ipProcessor = NewNullIPProcessor()
	}

	// 初始化流量控制器
	if options.FlowControllerConfig != nil && options.FlowControllerConfig.Client != nil {
		// 先创建IP记录器
		ipRecorder := flowcontroller.NewMongoIPRecorder(
			options.FlowControllerConfig.Client,
			options.FlowControllerConfig.Database,
			10000, // 默认容量
			a.Logger,
		)
		app.ipRecorder = ipRecorder

		// 创建流量控制器
		flowController, err := flowcontroller.NewFlowControllerFromMongoConfig(
			options.FlowControllerConfig.Client,
			options.FlowControllerConfig.Database,
			a.Logger,
			ipRecorder,
		)
		if err != nil {
			a.Logger.Warn().Err(err).Msg("初始化流量控制器失败")
		} else {
			app.flowController = flowController
			if err := app.flowController.Initialize(); err != nil {
				a.Logger.Warn().Err(err).Msg("流量控制器初始化失败")
			}
		}
	}

	debugLogger := debuglog.Default().
		WithLevel(debuglog.LevelDebug).
		WithOutput(os.Stdout)

	var config coraza.WAFConfig
	switch {
	case isDev && isDebug:
		config = coraza.NewWAFConfig().
			WithDirectives(a.Directives).
			WithErrorCallback(app.logCallback).
			WithDebugLogger(debugLogger).
			WithRootFS(mergefs.Merge(coreruleset.FS, io.OSFS))
	case isDebug:
		config = coraza.NewWAFConfig().
			WithDirectives(a.Directives).
			WithErrorCallback(app.logCallback).
			WithRootFS(mergefs.Merge(coreruleset.FS, io.OSFS))
	default:
		config = coraza.NewWAFConfig().
			WithDirectives(a.Directives).
			WithRootFS(mergefs.Merge(coreruleset.FS, io.OSFS))
	}

	waf, err := coraza.NewWAF(config)
	if err != nil {
		return nil, err
	}
	app.waf = waf

	const defaultExpire = time.Second * 10
	const defaultEvictionInterval = time.Second * 1

	app.cache = cache.NewTTLWithCallback(defaultExpire, defaultEvictionInterval, func(key, value any) {
		// 当transaction超时时关闭它
		t := value.(*transaction)
		if !t.m.TryLock() {
			// 我们在竞争中失败，事务已经在其他地方使用
			a.Logger.Info().Str("tx", t.tx.ID()).Msg("eviction called on currently used transaction")
			return
		}

		// 超时回调只负责清理资源，不再检查中断和记录日志
		// 因为如果事务中断，应该在请求或响应处理阶段就已经记录了日志

		// Process Logging won't do anything if TX was already logged.
		t.tx.ProcessLogging()
		if err := t.tx.Close(); err != nil {
			a.Logger.Error().Err(err).Str("tx", t.tx.ID()).Msg("error closing transaction")
		}
	})

	return app, nil
}

// NewDefaultApplication creates a new Application with background context
func (a AppConfig) NewApplication(options ApplicationOptions) (*Application, error) {
	return a.NewApplicationWithContext(context.Background(), options, false)
}

func (a *Application) logCallback(mr types.MatchedRule) {
	var l *zerolog.Event

	switch mr.Rule().Severity() {
	case types.RuleSeverityWarning:
		l = a.Logger.Warn()
	case types.RuleSeverityNotice,
		types.RuleSeverityInfo:
		l = a.Logger.Info()
	case types.RuleSeverityDebug:
		l = a.Logger.Debug()
	default:
		l = a.Logger.Error()
	}
	l.Msg(mr.ErrorLog())
}

type ErrInterrupted struct {
	Interruption *types.Interruption
}

func (e ErrInterrupted) Error() string {
	return fmt.Sprintf("interrupted with status %d and action %s", e.Interruption.Status, e.Interruption.Action)
}

func (e ErrInterrupted) Is(target error) bool {
	t, ok := target.(*ErrInterrupted)
	if !ok {
		return false
	}

	// 首先检查两个指针是否都为nil
	if e.Interruption == nil || t.Interruption == nil {
		return e.Interruption == t.Interruption
	}

	// 比较Interruption结构体的字段值
	return e.Interruption.RuleID == t.Interruption.RuleID &&
		e.Interruption.Action == t.Interruption.Action &&
		e.Interruption.Status == t.Interruption.Status &&
		e.Interruption.Data == t.Interruption.Data
}

// 优化的getHeaderValue函数 - 高性能版本
func getHeaderValue(headers []byte, targetHeader string) (string, error) {
	if len(headers) == 0 || targetHeader == "" {
		return "", nil
	}

	// 预先转换目标头部为小写，避免在循环中重复转换
	targetHeaderLower := strings.ToLower(targetHeader)
	targetLen := len(targetHeaderLower)

	start := 0
	for start < len(headers) {
		// 查找行尾
		lineEnd := start
		for lineEnd < len(headers) && headers[lineEnd] != '\n' {
			lineEnd++
		}

		// 获取当前行并去除可能的\r
		line := headers[start:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// 跳过空行
		if len(line) == 0 {
			start = lineEnd + 1
			continue
		}

		// 查找冒号
		colonIdx := bytes.IndexByte(line, ':')
		if colonIdx <= 0 {
			start = lineEnd + 1
			continue
		}

		// 提取key，并检查长度
		key := bytes.TrimSpace(line[:colonIdx])
		if len(key) != targetLen {
			start = lineEnd + 1
			continue
		}

		// 快速不区分大小写比较（仅限ASCII）
		isMatch := true
		for i := 0; i < targetLen; i++ {
			a := key[i]
			if a >= 'A' && a <= 'Z' {
				a += 32 // 转小写
			}
			if a != targetHeaderLower[i] {
				isMatch = false
				break
			}
		}

		if isMatch {
			value := bytes.TrimSpace(line[colonIdx+1:])
			return string(value), nil
		}

		// 移动到下一行
		start = lineEnd + 1
	}

	return "", nil
}

func getHostFromRequest(req *applicationRequest) string {
	if host, err := getHeaderValue(req.Headers, "host"); err == nil && host != "" {
		// 分离主机名和端口号
		if colonIndex := strings.Index(host, ":"); colonIndex != -1 {
			return host[:colonIndex]
		}
		return host
	}
	// 如果目标IP也可能包含端口，也做分离处理
	dstIpStr := req.DstIp.String()
	if colonIndex := strings.Index(dstIpStr, ":"); colonIndex != -1 {
		return dstIpStr[:colonIndex]
	}
	return dstIpStr
}

// getRealClientIP 从多种HTTP头部获取客户端真实IP (优化版本)
func getRealClientIP(req *applicationRequest) string {
	if req == nil {
		return ""
	}

	headers := req.Headers
	if len(headers) == 0 {
		// 如果没有header，直接返回源IP
		if req.SrcIp.IsValid() {
			return req.SrcIp.String()
		}
		return ""
	}

	// 快速路径：优先查找最常见的 X-Forwarded-For header
	if ip := getXForwardedForIP(headers); ip != "" {
		return ip
	}

	// 按优先级尝试其他头部
	priorityHeaders := []string{
		"x-real-ip",        // Nginx常用
		"true-client-ip",   // Akamai
		"cf-connecting-ip", // Cloudflare
		"fastly-client-ip", // Fastly
		"x-client-ip",      // 通用
	}

	// 批量查找简单headers（直接返回值的）
	for _, header := range priorityHeaders {
		if value, err := getHeaderValue(headers, header); err == nil && value != "" {
			if ip := strings.TrimSpace(value); ip != "" {
				return ip
			}
		}
	}

	// 查找复杂headers
	if value, err := getHeaderValue(headers, "x-original-forwarded-for"); err == nil && value != "" {
		if ips := strings.Split(value, ","); len(ips) > 0 {
			if ip := strings.TrimSpace(ips[0]); ip != "" {
				return ip
			}
		}
	}

	// Forwarded header需要特殊解析
	if value, err := getHeaderValue(headers, "forwarded"); err == nil && value != "" {
		if ip := parseForwardedHeaderFast(value); ip != "" {
			return ip
		}
	}

	// X-Cluster-Client-IP（最后检查）
	if value, err := getHeaderValue(headers, "x-cluster-client-ip"); err == nil && value != "" {
		if ip := strings.TrimSpace(value); ip != "" {
			return ip
		}
	}

	// 如果所有头部都没有，返回源IP
	if req.SrcIp.IsValid() {
		return req.SrcIp.String()
	}

	return ""
}

// getXForwardedForIP 快速解析X-Forwarded-For header
func getXForwardedForIP(headers []byte) string {
	// 快速查找 "x-forwarded-for:" 或 "X-Forwarded-For:"
	target := []byte("x-forwarded-for:")
	targetUpper := []byte("X-Forwarded-For:")

	start := 0
	for start < len(headers) {
		// 查找行尾
		lineEnd := start
		for lineEnd < len(headers) && headers[lineEnd] != '\n' {
			lineEnd++
		}

		line := headers[start:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// 检查是否匹配 X-Forwarded-For
		if len(line) > 16 { // "x-forwarded-for:" 最小长度是16
			// 快速检查前缀
			if (bytes.HasPrefix(line, target) || bytes.HasPrefix(line, targetUpper)) ||
				(len(line) > 15 && isXForwardedForHeader(line)) {

				// 找到冒号后的值
				colonIdx := bytes.IndexByte(line, ':')
				if colonIdx > 0 && colonIdx < len(line)-1 {
					value := bytes.TrimSpace(line[colonIdx+1:])
					if len(value) > 0 {
						// 提取第一个IP（逗号分隔）
						valueStr := string(value)
						if commaIdx := strings.Index(valueStr, ","); commaIdx > 0 {
							ip := strings.TrimSpace(valueStr[:commaIdx])
							if ip != "" {
								return ip
							}
						} else {
							ip := strings.TrimSpace(valueStr)
							if ip != "" {
								return ip
							}
						}
					}
				}
			}
		}

		start = lineEnd + 1
	}
	return ""
}

// isXForwardedForHeader 检查是否是X-Forwarded-For header（不区分大小写）
func isXForwardedForHeader(line []byte) bool {
	if len(line) < 16 { // "x-forwarded-for:" 长度是16
		return false
	}

	target := "x-forwarded-for:"
	for i := 0; i < 16; i++ {
		c := line[i]
		if c >= 'A' && c <= 'Z' {
			c += 32 // 转小写
		}
		if c != target[i] {
			return false
		}
	}
	return true
}

// parseForwardedHeaderFast 快速解析Forwarded header
func parseForwardedHeaderFast(forwarded string) string {
	// 快速查找 "for=" 模式
	forPrefix := "for="
	start := 0

	for {
		idx := strings.Index(forwarded[start:], forPrefix)
		if idx == -1 {
			break
		}

		start += idx + 4 // len("for=")
		if start >= len(forwarded) {
			break
		}

		// 查找值的结束位置（分号或字符串结尾）
		end := start
		for end < len(forwarded) && forwarded[end] != ';' {
			end++
		}

		if end > start {
			ip := strings.TrimSpace(forwarded[start:end])
			// 去除引号
			ip = strings.Trim(ip, "\"")

			// 处理IPv6地址格式 [ip]:port 或 [ip]
			if strings.HasPrefix(ip, "[") {
				if closeBracket := strings.Index(ip, "]"); closeBracket > 1 {
					ip = ip[1:closeBracket]
				}
			}

			if ip != "" {
				return ip
			}
		}

		start = end
	}

	return ""
}

// buildURLFromBytes 高性能 URL 构建函数
func buildURLFromBytes(path, query []byte) string {
	if len(query) == 0 {
		return string(path)
	}

	// 一次性分配所需内存
	result := make([]byte, 0, len(path)+1+len(query))
	result = append(result, path...)
	result = append(result, '?')
	result = append(result, query...)
	return string(result)
}

// buildFullURL 构建完整 URL（包含 host）
func buildFullURL(host string, path, query []byte) string {
	// 计算总长度
	totalLen := len(host) + len(path)
	if len(query) > 0 {
		totalLen += 1 + len(query) // +1 for '?'
	}

	// 一次性分配
	result := make([]byte, 0, totalLen)
	result = append(result, host...)
	result = append(result, path...)
	if len(query) > 0 {
		result = append(result, '?')
		result = append(result, query...)
	}
	return string(result)
}
