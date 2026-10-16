package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	timeout     = 500 * time.Millisecond // 扫描拨号超时
	maxDuration = 2 * time.Second        // 扫描 HTTP 会话最大持续时间
)

var (
	activeConnections int32 // 用于跟踪活跃连接的数量
	randomMu          sync.Mutex
	randomGenerator   = rand.New(rand.NewSource(time.Now().UnixNano()))
	scanning          int32 // 原子标志：1 表示后台重扫进行中
)

// IPManager 用于安全管理 IP 池状态
// 设计：池一旦建立就持续服务；重扫在后台进行，完成后原子替换整个池
type IPManager struct {
	mu          sync.RWMutex
	currentIP   string
	currentIdx  int
	ipAddresses []string
	statuses    map[string]bool          // 最近一轮池内探测结果，true=可用
	rtts        map[string]time.Duration // 最近一轮池内探测的 RTT，缺失=失效/超延迟
}

func NewIPManager() *IPManager {
	return &IPManager{statuses: make(map[string]bool), rtts: make(map[string]time.Duration)}
}

// Replace 用新池原子替换旧池；新池成员默认视为可用（已通过连通性验证）
func (m *IPManager) Replace(ips []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ipAddresses = ips
	m.statuses = make(map[string]bool, len(ips))
	for _, ip := range ips {
		m.statuses[ip] = true
	}
	if len(ips) > 0 {
		m.currentIP = ips[0]
		m.currentIdx = 0
	} else {
		m.currentIP = ""
		m.currentIdx = -1
	}
}

func (m *IPManager) GetCurrentIP() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.currentIP
}

func (m *IPManager) GetIPAddresses() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.ipAddresses...)
}

// SetStatuses 写入最近一轮探测结果
func (m *IPManager) SetStatuses(statuses map[string]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses = statuses
}

// SetRTTs 写入最近一轮探测的 RTT（缺失 = 失效/超延迟）
func (m *IPManager) SetRTTs(rtts map[string]time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rtts = rtts
}

// FastestCandidate 在本轮探测结果中寻找显著快于当前 IP 的候选：
// RTT 比当前低 30% 以上且绝对差距 >= 50ms，返回其中最快者
func (m *IPManager) FastestCandidate(rtts map[string]time.Duration) (ip string, bestRTT, curRTT time.Duration, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.currentIP == "" {
		return "", 0, 0, false
	}
	cur, alive := rtts[m.currentIP]
	if !alive {
		return "", 0, 0, false // 当前 IP 本轮失效，由失效切换逻辑处理
	}
	for candidate, r := range rtts {
		if candidate == m.currentIP {
			continue
		}
		if r*10 < cur*7 && cur-r >= 50*time.Millisecond {
			if ip == "" || r < bestRTT {
				ip, bestRTT = candidate, r
			}
		}
	}
	if ip == "" {
		return "", 0, 0, false
	}
	return ip, bestRTT, cur, true
}

// SwitchToIP 将当前 IP 切换为池内指定 IP
func (m *IPManager) SwitchToIP(target string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, ip := range m.ipAddresses {
		if ip == target {
			log.Printf("择优切换: %s -> %s", m.currentIP, target)
			m.currentIP = target
			m.currentIdx = i
			return
		}
	}
}

// switchToNextHealthyIP 从当前索引之后顺序实拨测试，切到第一个可连通的 IP
// 不做任何 HTTP 校验，只验证 TCP 可达且 RTT 在 delay 内
func (m *IPManager) switchToNextHealthyIP(port int, delay time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.ipAddresses)
	if n == 0 {
		return false
	}
	for i := 1; i < n; i++ {
		idx := (m.currentIdx + i) % n
		ip := m.ipAddresses[idx]
		if tcpOK(ip, port, delay) {
			log.Printf("切换到新的有效 IP: %s (索引 %d)", ip, idx)
			m.currentIP = ip
			m.currentIdx = idx
			return true
		}
	}
	log.Println("池内没有其他可连通的 IP，保持当前 IP")
	return false
}

type result struct {
	ip          string        // IP地址
	dataCenter  string        // 数据中心
	region      string        // 地区
	city        string        // 城市
	latency     string        // 延迟
	tcpDuration time.Duration // TCP请求延迟
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

// scanOpts 汇总扫描与建池所需的配置
type scanOpts struct {
	coloFilter string
	ipnum      int
	ipsType    string
	random     bool
	task       int
	port       int
	delay      time.Duration
}

func main() {
	localAddr := flag.String("addr", "0.0.0.0:1234", "本地监听的 IP 和端口")
	code := flag.Int("code", 200, "HTTP/HTTPS 响应状态码")
	coloFilter := flag.String("colo", "", "筛选数据中心例如 HKG,SJC,LAX (多个数据中心用逗号隔开,留空则忽略匹配)")
	Delay := flag.Int("delay", 300, "有效延迟（毫秒），超过此延迟将断开连接")
	domain := flag.String("domain", "cloudflaremirrors.com/debian", "响应状态码检查的域名地址")
	ipCount := flag.Int("ipnum", 20, "提取的有效IP数量（池大小）")
	ipsType := flag.String("ips", "4", "指定生成IPv4还是IPv6地址 (4或6)")
	num := flag.Int("num", 5, "目标负载 IP 数量")
	port := flag.Int("port", 443, "转发的目标端口")
	probe := flag.Int("probe", 60, "池内探测间隔（秒）")
	random := flag.Bool("random", true, "是否随机生成IP，如果为false，则从CIDR中拆分出所有IP")
	maxThreads := flag.Int("task", 100, "并发请求最大协程数")
	useTLS := flag.Bool("tls", true, "是否为 TLS 端口")

	flag.Parse()

	// 以下参数保留用于命令行兼容，新质量判定逻辑不依赖它们
	_ = code
	_ = domain
	_ = useTLS

	delayDur := time.Duration(*Delay) * time.Millisecond

	ipManager := NewIPManager()

	// 先监听再建池，保证 :1234 尽早可连接
	listener, err := net.Listen("tcp", *localAddr)
	if err != nil {
		log.Fatalf("无法监听 %s: %v", *localAddr, err)
	}
	defer listener.Close()

	log.Printf("正在监听 %s 并转发到目标端口 %d，有效延迟：%d ms，池大小：%d，探测间隔：%d s",
		*localAddr, *port, *Delay, *ipCount, *probe)

	locations, err := loadLocations()
	if err != nil {
		log.Fatalf("加载位置信息失败: %v", err)
	}
	locationMap := make(map[string]location, len(locations))
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	ipsFile, err := ensureIPSource(*ipsType)
	if err != nil {
		log.Fatalf("获取 IP 源列表失败: %v", err)
	}

	opts := scanOpts{
		coloFilter: *coloFilter,
		ipnum:      *ipCount,
		ipsType:    *ipsType,
		random:     *random,
		task:       *maxThreads,
		port:       *port,
		delay:      delayDur,
	}

	// 初始建池（阻塞直到拿到第一个可用池，扫描期间端口已监听但尚无可用 IP）
	log.Println("开始初始扫描建池 ...")
	rescanPool(ipManager, locationMap, ipsFile, opts)

	hintCh := make(chan struct{}, 1)

	// 常驻 accept：客户端连接始终基于当前 IP 转发
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				log.Printf("接受连接时发生错误: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}

			clientAddr := conn.RemoteAddr().String()
			atomic.AddInt32(&activeConnections, 1)
			log.Printf("客户端来源: %s 连接建立，当前活跃连接数: %d", clientAddr, atomic.LoadInt32(&activeConnections))

			go handleConnection(conn, generateTargets(ipManager.GetCurrentIP(), *port, *num), delayDur, hintCh)
		}
	}()

	// hint 消费：客户端连接拨不通当前 IP 时快速切换（不等下一轮探测）
	go func() {
		for range hintCh {
			cur := ipManager.GetCurrentIP()
			if cur != "" && tcpOK(cur, *port, delayDur) {
				continue // 偶发失败，当前 IP 仍健康
			}
			ipManager.switchToNextHealthyIP(*port, delayDur)
		}
	}()

	// 池监控：定期探测全池，过半质量不足则后台重扫
	go monitorLoop(ipManager, *port, delayDur, time.Duration(*probe)*time.Second, func() {
		rescanPool(ipManager, locationMap, ipsFile, opts)
	})

	select {} // 常驻运行
}

// ensureIPSource 确保 IP 源文件存在（不存在则下载），返回文件名
func ensureIPSource(ipsType string) (string, error) {
	var filename, url string
	switch ipsType {
	case "6":
		filename, url = "ips-v6.txt", "https://www.baipiao.eu.org/cloudflare/ips-v6"
	case "4":
		filename, url = "ips-v4.txt", "https://www.baipiao.eu.org/cloudflare/ips-v4"
	default:
		return "", fmt.Errorf("无效的IP类型: %s (请使用 4 或 6)", ipsType)
	}
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		fmt.Printf("文件 %s 不存在，正在从 URL %s 下载数据\n", filename, url)
		content, err := getURLContent(url)
		if err != nil {
			return "", fmt.Errorf("获取URL内容出错: %v", err)
		}
		if err := saveToFile(filename, content); err != nil {
			return "", fmt.Errorf("保存文件出错: %v", err)
		}
	}
	return filename, nil
}

// buildCandidateIPs 根据配置生成本轮扫描的候选 IP 列表
func buildCandidateIPs(ipsFile string, opts scanOpts) ([]string, error) {
	content, err := getFileContent(ipsFile)
	if err != nil {
		return nil, fmt.Errorf("读取本地文件出错: %v", err)
	}
	if opts.random {
		ipList := parseIPList(content)
		if opts.ipsType == "6" {
			return getRandomIPv6s(ipList), nil
		}
		return getRandomIPv4s(ipList), nil
	}
	return readIPs(ipsFile)
}

// rescanPool 执行一次完整扫描并原子替换 IP 池
// 扫描期间旧池继续服务；结果为 0 时退避 30 秒重试直到成功
func rescanPool(m *IPManager, locationMap map[string]location, ipsFile string, opts scanOpts) {
	for {
		startTime := time.Now()

		ipList, err := buildCandidateIPs(ipsFile, opts)
		if err != nil {
			log.Printf("%v，30 秒后重试", err)
			time.Sleep(30 * time.Second)
			continue
		}

		results := scanIPs(ipList, locationMap, opts.task)

		if len(results) == 0 {
			log.Printf("未发现有效IP，30 秒后重试")
			time.Sleep(30 * time.Second)
			continue
		}

		// 应用数据中心筛选
		if opts.coloFilter != "" {
			filters := strings.Split(opts.coloFilter, ",")
			var filteredResults []result
			for _, r := range results {
				for _, filter := range filters {
					if strings.EqualFold(r.dataCenter, filter) {
						filteredResults = append(filteredResults, r)
						break
					}
				}
			}
			results = filteredResults
			if len(results) == 0 {
				log.Printf("筛选数据中心 %s 后无有效IP，30 秒后重试", opts.coloFilter)
				time.Sleep(30 * time.Second)
				continue
			}
		}

		// 按 TCP 延迟排序，取前 ipnum 个
		sort.Slice(results, func(i, j int) bool {
			return results[i].tcpDuration < results[j].tcpDuration
		})
		if len(results) > opts.ipnum {
			results = results[:opts.ipnum]
		}

		// 目标端口连通性验证：保证入池的每个 IP 都可连通且延迟达标
		var ips []string
		for _, r := range results {
			if tcpOK(r.ip, opts.port, opts.delay) {
				ips = append(ips, r.ip)
			}
		}
		if len(ips) == 0 {
			log.Printf("候选 IP 均无法在 %d ms 内连通目标端口，30 秒后重试", opts.delay.Milliseconds())
			time.Sleep(30 * time.Second)
			continue
		}

		// 原子替换池，服务无缝切换到新池
		m.Replace(ips)
		log.Printf("新 IP 池已生效，共 %d 个 IP（扫描+验证耗时 %d 秒）",
			len(ips), time.Since(startTime)/time.Second)

		fmt.Println("IP 地址 | 数据中心 | 地区 | 城市 | 延迟")
		for _, r := range results {
			fmt.Printf("%s | %s | %s | %s | %s\n", r.ip, r.dataCenter, r.region, r.city, r.latency)
		}
		return
	}
}

// monitorLoop 定期探测池内全部 IP：
//   - 某轮探测中过半 IP 不可用/超延迟 → 触发一次后台全池重扫（扫描期间不重复触发）
//   - 当前 IP 在探测中失效 → 立即切换到池内下一个可连通 IP
//   - 池内存在显著更快的 IP（RTT 低 30% 且快 50ms 以上）且连续 2 轮保持 → 择优切换当前 IP
func monitorLoop(m *IPManager, port int, delay time.Duration, interval time.Duration, triggerRescan func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	fastStreak := 0 // 连续满足择优条件的轮数

	for range ticker.C {
		ips := m.GetIPAddresses()
		if len(ips) == 0 {
			continue
		}

		rtts := probePool(ips, port, delay)
		m.SetRTTs(rtts)

		statuses := make(map[string]bool, len(ips))
		bad := 0
		for _, ip := range ips {
			_, ok := rtts[ip]
			statuses[ip] = ok
			if !ok {
				bad++
			}
		}
		m.SetStatuses(statuses)

		cur := m.GetCurrentIP()
		if _, curAlive := rtts[cur]; !curAlive {
			// 当前 IP 失效 → 立即切换
			log.Printf("当前 IP %s 探测失效", cur)
			m.switchToNextHealthyIP(port, delay)
			fastStreak = 0
		} else if bestIP, bestRTT, curRTT, ok := m.FastestCandidate(rtts); ok {
			// 择优切换：连续 2 轮显著更快才切，防单轮网络抖动
			fastStreak++
			log.Printf("池内存在更快 IP: %s(%dms) vs 当前 %s(%dms)，连续第 %d 轮",
				bestIP, bestRTT.Milliseconds(), cur, curRTT.Milliseconds(), fastStreak)
			if fastStreak >= 2 {
				m.SwitchToIP(bestIP)
				fastStreak = 0
			}
		} else {
			fastStreak = 0
		}

		// 过半质量不足 → 后台重扫（atomic 标志防重复触发）
		if bad*2 > len(ips) {
			if atomic.CompareAndSwapInt32(&scanning, 0, 1) {
				log.Printf("池内 %d/%d 个 IP 质量不足，启动后台重扫（旧池继续服务）", bad, len(ips))
				go func() {
					defer atomic.StoreInt32(&scanning, 0)
					triggerRescan()
				}()
			} else {
				log.Printf("池内 %d/%d 个 IP 质量不足，重扫已在进行中，跳过", bad, len(ips))
			}
		}
	}
}

// probePool 并发探测池内所有 IP：返回拨号成功的 RTT（失败或 RTT>delay 的 IP 不入结果）
func probePool(ips []string, port int, delay time.Duration) map[string]time.Duration {
	var wg sync.WaitGroup
	var mu sync.Mutex
	rtts := make(map[string]time.Duration, len(ips))

	for _, ip := range ips {
		wg.Add(1)
		go func(ipAddr string) {
			defer wg.Done()
			start := time.Now()
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ipAddr, fmt.Sprintf("%d", port)), delay)
			if err != nil {
				return
			}
			rtt := time.Since(start)
			conn.Close()
			if rtt <= delay {
				mu.Lock()
				rtts[ipAddr] = rtt
				mu.Unlock()
			}
		}(ip)
	}

	wg.Wait()
	return rtts
}

// tcpOK 验证 IP:port TCP 可达且 RTT 在 delay 内
func tcpOK(ip string, port int, delay time.Duration) bool {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, fmt.Sprintf("%d", port)), delay)
	if err != nil {
		return false
	}
	conn.Close()
	return time.Since(start) <= delay
}

// loadLocations 加载位置信息，使用函数封装确保 defer 正确执行
func loadLocations() ([]location, error) {
	var locations []location

	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("本地 locations.json 不存在\n正在从 https://www.baipiao.eu.org/cloudflare/locations 下载 locations.json")
		resp, err := http.Get("https://www.baipiao.eu.org/cloudflare/locations")
		if err != nil {
			return nil, fmt.Errorf("无法从URL中获取JSON: %v", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("无法读取响应体: %v", err)
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}

		file, err := os.Create("locations.json")
		if err != nil {
			return nil, fmt.Errorf("无法创建文件: %v", err)
		}
		defer file.Close()

		_, err = file.Write(body)
		if err != nil {
			return nil, fmt.Errorf("无法写入文件: %v", err)
		}
	} else {
		file, err := os.Open("locations.json")
		if err != nil {
			return nil, fmt.Errorf("无法打开文件: %v", err)
		}
		defer file.Close()

		body, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("无法读取文件: %v", err)
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			return nil, fmt.Errorf("无法解析JSON: %v", err)
		}
	}

	return locations, nil
}

// scanIPs 扫描 IP 列表并返回结果
func scanIPs(ipList []string, locationMap map[string]location, maxThreads int) []result {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []result

	thread := make(chan struct{}, maxThreads)

	var count int32
	total := len(ipList)

	for _, ip := range ipList {
		wg.Add(1)
		thread <- struct{}{}
		go func(ipAddr string) {
			defer func() {
				<-thread
				wg.Done()
				current := atomic.AddInt32(&count, 1)
				percentage := float64(current) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", current, total, percentage)
				if int(current) == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", current, total, percentage)
				}
			}()

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ipAddr, "80"))
			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)

			// 通过根路径响应头里的 CF-RAY 提取机房信息
			requestURL := "http://" + net.JoinHostPort(ipAddr, "80")
			req, err := http.NewRequest("GET", requestURL, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true

			conn.SetDeadline(time.Now().Add(maxDuration))
			err = req.Write(conn)
			if err != nil {
				return
			}

			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, req)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			cfRay := strings.TrimSpace(resp.Header.Get("CF-RAY"))
			if cfRay == "" {
				return
			}

			parts := strings.Split(cfRay, "-")
			if len(parts) < 2 {
				return
			}

			dataCenter := strings.TrimSpace(parts[len(parts)-1])
			if dataCenter == "" {
				return
			}

			loc, ok := locationMap[dataCenter]
			mu.Lock()
			if ok {
				fmt.Printf("发现有效IP %s 位置信息 %s 延迟 %d 毫秒\n", ipAddr, loc.City, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			} else {
				fmt.Printf("发现有效IP %s 位置信息未知 延迟 %d 毫秒\n", ipAddr, tcpDuration.Milliseconds())
				results = append(results, result{ipAddr, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration})
			}
			mu.Unlock()
		}(ip)
	}

	wg.Wait()
	return results
}

// 获取URL内容
func getURLContent(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			content.WriteString(line + "\n")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	return content.String(), nil
}

// 从本地文件读取内容
func getFileContent(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// 将内容保存到本地文件
func saveToFile(filename, content string) error {
	return os.WriteFile(filename, []byte(content), 0644)
}

// 解析IP列表，跳过空行
func parseIPList(content string) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var ipList []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			ipList = append(ipList, line)
		}
	}
	return ipList
}

func nextRandomIntn(n int) int {
	randomMu.Lock()
	defer randomMu.Unlock()
	return randomGenerator.Intn(n)
}

// 从每个/24子网随机提取一个IPv4
func getRandomIPv4s(ipList []string) []string {
	var randomIPs []string
	for _, subnet := range ipList {
		// 跳过空行
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		baseIP := strings.TrimSuffix(subnet, "/24")
		octets := strings.Split(baseIP, ".")
		if len(octets) >= 4 {
			octets[3] = fmt.Sprintf("%d", nextRandomIntn(256))
			randomIP := strings.Join(octets, ".")
			randomIPs = append(randomIPs, randomIP)
		}
	}
	return randomIPs
}

// 从每个/48子网随机提取一个IPv6
func getRandomIPv6s(ipList []string) []string {
	var randomIPs []string
	for _, subnet := range ipList {
		// 跳过空行
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		baseIP := strings.TrimSuffix(subnet, "/48")
		sections := strings.Split(baseIP, ":")
		if len(sections) >= 3 {
			sections = sections[:3]
			for i := 3; i < 8; i++ {
				sections = append(sections, fmt.Sprintf("%x", nextRandomIntn(65536)))
			}
			randomIP := strings.Join(sections, ":")
			randomIPs = append(randomIPs, randomIP)
		}
	}
	return randomIPs
}

// 从CIDR中拆分出所有IP
func readIPs(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 跳过空行
		if line == "" {
			continue
		}
		if strings.Contains(line, "/") {
			ipAddr, ipNet, err := net.ParseCIDR(line)
			if err != nil {
				return nil, err
			}
			// 使用新变量避免遮蔽
			for currentIP := ipAddr.Mask(ipNet.Mask); ipNet.Contains(currentIP); incrementIP(currentIP) {
				ips = append(ips, currentIP.String())
			}
		} else {
			ips = append(ips, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return ips, nil
}

// 增加IP
func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func generateTargets(ip string, port int, num int) []string {
	targets := make([]string, num)
	address := ip
	if strings.Contains(ip, ":") {
		address = fmt.Sprintf("[%s]", ip)
	}
	for i := 0; i < num; i++ {
		targets[i] = fmt.Sprintf("%s:%d", address, port)
	}
	return targets
}

// 处理客户端连接，尝试连接到指定的转发地址，并选择延迟最低的连接
// 全部目标拨不通时发 hint 信号，触发当前 IP 快速切换
func handleConnection(conn net.Conn, forwardAddrs []string, delay time.Duration, hint chan<- struct{}) {
	defer func() {
		clientAddr := conn.RemoteAddr().String()
		atomic.AddInt32(&activeConnections, -1)
		log.Printf("客户端来源: %s 连接关闭，当前活跃连接数: %d", clientAddr, atomic.LoadInt32(&activeConnections))
		conn.Close()
	}()

	type connResult struct {
		conn   net.Conn
		addr   string
		delay  time.Duration
		errMsg string
	}

	results := make(chan connResult, len(forwardAddrs))

	// 并发尝试连接每个转发地址
	for _, addr := range forwardAddrs {
		go func(targetAddr string) {
			start := time.Now()
			forwardConn, err := net.DialTimeout("tcp", targetAddr, delay)
			elapsed := time.Since(start)

			if err != nil {
				results <- connResult{nil, targetAddr, elapsed, fmt.Sprintf("连接到 %s 的延迟超过有效值 %d ms", targetAddr, delay.Milliseconds())}
				return
			}

			results <- connResult{forwardConn, targetAddr, elapsed, ""}
		}(addr)
	}

	var validConns []connResult
	var bestConn net.Conn
	var bestDelay time.Duration
	var bestAddr string

	// 收集结果并找到延迟最低的有效连接
	for i := 0; i < len(forwardAddrs); i++ {
		res := <-results
		if res.conn != nil {
			validConns = append(validConns, res)

			if bestConn == nil || res.delay < bestDelay {
				if bestConn != nil {
					bestConn.Close()
				}
				bestConn = res.conn
				bestDelay = res.delay
				bestAddr = res.addr
			} else {
				res.conn.Close()
			}
		} else {
			log.Printf("错误: %s", res.errMsg)
		}
	}

	log.Println("符合要求的连接:")
	for _, vc := range validConns {
		log.Printf("地址: %s 延迟: %d ms", vc.addr, vc.delay.Milliseconds())
	}

	// 如果找到最佳连接，开始转发数据
	if bestConn != nil {
		log.Printf("选择最佳连接: 地址: %s 延迟: %d ms", bestAddr, bestDelay.Milliseconds())
		pipeConnections(conn, bestConn)
	} else {
		log.Println("未找到符合延迟要求的连接，关闭客户端连接")
		select {
		case hint <- struct{}{}:
		default:
		}
	}
}

func pipeConnections(src, dst net.Conn) {
	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			src.Close()
			dst.Close()
		})
	}

	wg.Add(2)

	go func() {
		defer wg.Done()
		_, _ = io.Copy(src, dst)
		closeBoth()
	}()

	go func() {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		closeBoth()
	}()

	wg.Wait()
}
