package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

var (
	host      string
	port      int
	timeout   time.Duration
	count     int
	interval  time.Duration
	parallel  int
	jsonOut   string
	colorMode bool
	showTTL   bool
	// doTrace   bool // 移除 trace 选项
)

// Statistics 结构体用于存储 TCPing 统计信息
type Statistics struct {
	Target    string          // 目标地址
	Total     atomic.Int32    // 总请求数
	Success   atomic.Int32    // 成功请求数
	Failed    atomic.Int32    // 失败请求数
	Min       atomic.Int64    // 最小延迟 (纳秒)
	Max       atomic.Int64    // 最大延迟 (纳秒)
	Sum       atomic.Int64    // 总延迟 (纳秒)
	StartTime time.Time       // 测试开始时间
	Latencies []time.Duration // 所有成功请求的延迟列表
	TTLs      map[int]int     // TTL 值统计
	mutex     sync.Mutex      // 互斥锁，用于保护统计信息并发安全
}

func main() {
	// 解析命令行参数
	parseFlags()
	// 验证命令行参数的有效性
	validateArgs()

	// 构建目标地址字符串，格式为 "host:port"
	target := buildTarget()
	// 初始化统计结构体
	stats := &Statistics{
		Target:    target,
		StartTime: time.Now(),
		Latencies: make([]time.Duration, 0, count), // 初始化延迟切片
		TTLs:      make(map[int]int),               // 初始化 TTL 统计 map
		// Hops:      make([]Hop, 0), // 初始化 Hops 切片, 移除
	}
	// 设置中断处理函数，监听 Ctrl+C 等中断信号
	setupInterruptHandler(stats)

	// 打印程序头部信息，显示目标、端口、超时等配置
	printHeader(target)

	// 创建一个通道 (channel) 来限制并发请求的数量
	sem := make(chan struct{}, parallel)
	// 创建一个 WaitGroup 来等待所有 worker 协程完成
	var wg sync.WaitGroup

	// 循环发送请求，直到达到指定的请求次数 (count) 或无限循环 (count=0)
	for seq := 1; count == 0 || seq <= count; {
		select {
		case sem <- struct{}{}: // 尝试向信号量通道发送数据，如果通道未满则发送成功
			// 请求通道有空闲，开始一个新的请求
			wg.Add(1) // 增加 WaitGroup 的计数器
			go func(s int) {
				// worker 协程函数
				defer func() {
					<-sem     // 释放信号量通道资源，允许新的请求开始
					wg.Done() // worker 协程完成，减少 WaitGroup 的计数器
				}()
				// 捕获并处理子协程中的 panic 异常，防止程序崩溃
				if r := recover(); r != nil {
					stats.mutex.Lock() // 加锁，保护 stats 结构体
					fmt.Printf("%s[ERROR] panic: %v%s\n", colorRed, r, colorReset)
					stats.mutex.Unlock() // 解锁
				}
				// 执行 worker 函数发送 TCPing 请求
				worker(target, stats, s)
				// 根据设定的请求间隔时间休眠
				time.Sleep(interval)
			}(seq)
			seq++
		default:
			// 请求通道已满，短暂休眠后重试，避免过度消耗 CPU
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 等待所有 worker 协程完成
	wg.Wait()
	// 所有请求完成后，输出统计结果
	outputResults(stats)
}

// parseFlags 解析命令行参数
func parseFlags() {
	// 配置选项参数，使用 flag 标准库
	flag.DurationVar(&timeout, "t", 2*time.Second, "连接超时时间")
	flag.IntVar(&count, "c", 0, "测试次数 (0=无限)")
	flag.DurationVar(&interval, "i", 1*time.Second, "请求间隔时间")
	flag.IntVar(&parallel, "P", 1, "并发连接数 (1-64)")
	flag.StringVar(&jsonOut, "o", "", "JSON输出文件路径")
	flag.BoolVar(&colorMode, "C", false, "启用彩色输出")
	flag.BoolVar(&showTTL, "ttl", false, "显示TTL值 (仅Unix系统)")
	// flag.BoolVar(&doTrace, "trace", false, "执行路由追踪 (traceroute)") // 移除 trace 选项

	// 自定义帮助信息，当用户输入 -h 或 --help 时显示
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使用方法: %s <目标主机> [端口] [选项]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "必选参数:")
		fmt.Fprintln(os.Stderr, "  <目标主机>\t需要测试的主机名或IP地址")
		fmt.Fprintln(os.Stderr, "可选参数:")
		fmt.Fprintln(os.Stderr, "  [端口]\t\t目标端口号 (默认为 80)")
		fmt.Fprintln(os.Stderr, "选项:")
		flag.PrintDefaults() // 打印默认选项帮助信息
	}

	// 手动解析参数，处理位置参数和选项参数
	args := os.Args[1:]

	// 分离位置参数和选项参数
	var positionalArgs []string
	for i := 0; i < len(args); {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			// 如果是选项参数，直接跳过，交给 flag.CommandLine.Parse 处理
			i++
			if f := flag.Lookup(arg[1:]); f != nil && f.Value.String() == "" {
				// 如果选项参数需要值，则跳过下一个参数 (选项的值)
				if i < len(args) && !strings.HasPrefix(args[i], "-") {
					i++
				}
			}
		} else {
			// 如果是位置参数 (目标主机和端口)，提取出来
			positionalArgs = append(positionalArgs, arg)
			args = append(args[:i], args[i+1:]...) // 从 args 中移除已处理的位置参数
		}
	}

	// 解析剩余的选项参数
	if err := flag.CommandLine.Parse(args); err != nil {
		exitWithError(err.Error()) // 解析出错则退出程序
	}

	// 验证位置参数，必须至少有一个目标主机
	if len(positionalArgs) < 1 {
		exitWithError("必须指定目标主机")
	}
	host = positionalArgs[0] // 第一个位置参数为主机名或 IP 地址

	// 设置端口号，如果提供了端口位置参数则使用，否则默认为 80
	port = 80 // 默认端口
	if len(positionalArgs) > 1 {
		portStr := positionalArgs[1] // 第二个位置参数为端口号
		var err error
		port, err = strconv.Atoi(portStr) // 字符串转换为整数
		if err != nil || port < 1 || port > 65535 {
			exitWithError("无效的端口号 (必须为 1-65535)")
		}
	}

	// 自动判断 IP 类型 (仅用于Debug输出和打印目标地址)
	parsedAddr, err := netip.ParseAddr(host)
	if err == nil { // 能够解析为主机名或 IP 地址
		fmt.Printf("%s[DEBUG] Target Address: %s%s\n", colorCyan, parsedAddr.String(), colorReset)
	}
}

// worker 函数执行 TCPing 请求
func worker(target string, stats *Statistics, seq int) {
	// 记录请求开始时间
	start := time.Now()
	networkType := "tcp" // 设置网络类型为 TCP
	dialTarget := target // 连接目标地址

	// 使用 net.DialTimeout 尝试建立 TCP 连接
	conn, err := net.DialTimeout(networkType, dialTarget, timeout)
	duration := time.Since(start) // 计算连接耗时

	stats.Total.Add(1) // 总请求数 +1
	if err != nil {
		// 连接失败
		stats.Failed.Add(1) // 失败请求数 +1
		printResult(stats, seq, duration, err, 0)
		return // 失败直接返回
	}
	defer conn.Close() // 确保连接关闭

	ttl := 0
	if showTTL {
		// 获取 TTL (Time-To-Live) 值
		if t, err := getTTL(conn); err == nil {
			ttl = t // 获取 TTL 成功
			stats.mutex.Lock()
			stats.TTLs[t]++ // 统计 TTL 值
			stats.mutex.Unlock()
		}
	}

	// 连接成功
	stats.mutex.Lock()
	stats.Latencies = append(stats.Latencies, duration) // 添加延迟到延迟列表
	stats.mutex.Unlock()
	stats.Success.Add(1)           // 成功请求数 +1
	stats.Sum.Add(int64(duration)) // 累加延迟
	updateMinMax(stats, duration)  // 更新最小和最大延迟
	printResult(stats, seq, duration, nil, ttl)
}

// updateMinMax 更新统计信息中的最小和最大延迟
func updateMinMax(stats *Statistics, d time.Duration) {
	// 循环更新最小值，使用原子操作保证并发安全
	for {
		oldMin := stats.Min.Load()
		if oldMin == 0 || d < time.Duration(oldMin) {
			// 当前延迟小于最小值，尝试更新
			if stats.Min.CompareAndSwap(oldMin, int64(d)) {
				break // 更新成功，退出循环
			}
		} else {
			break // 当前延迟不小于最小值，无需更新，退出循环
		}
	}

	// 循环更新最大值，使用原子操作保证并发安全
	for {
		oldMax := stats.Max.Load()
		if d > time.Duration(oldMax) {
			// 当前延迟大于最大值，尝试更新
			if stats.Max.CompareAndSwap(oldMax, int64(d)) {
				break // 更新成功，退出循环
			}
		} else {
			break // 当前延迟不大于最大值，无需更新，退出循环
		}
	}
}

// validateArgs 验证命令行参数的有效性
func validateArgs() {
	if host == "" {
		exitWithError("必须指定目标主机")
	}

	if port < 1 || port > 65535 {
		exitWithError("端口号无效")
	}

	if parallel < 1 || parallel > 64 {
		exitWithError("并发数必须在1-64之间")
	}
}

// buildTarget 构建目标地址字符串
func buildTarget() string {
	// 根据主机名是否包含端口和是否为IPv6地址，构建目标字符串
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%d", host, port) // IPv6 地址格式
	}
	return fmt.Sprintf("%s:%d", host, port) // IPv4 地址或主机名格式
}

// printHeader 打印 TCPing 头部信息
func printHeader(target string) {
	// 打印 TCPing 的头部信息，包括目标地址、端口、超时时间、并发数等
	c := colorCyan  // 青色
	r := colorReset // 重置颜色
	if !colorMode { // 如果未启用彩色模式，则设置为空字符串
		c, r = "", ""
	}
	fmt.Printf("%s[TCPing] 目标: %s 端口: %d 超时: %v 并发: %d%s\n",
		c, host, port, timeout, parallel, r)
}

// printResult 打印每次 TCPing 的结果
func printResult(stats *Statistics, seq int, duration time.Duration, err error, ttl int) {
	// 打印每次 TCPing 的结果，包括序列号、目标地址、端口、响应状态、TTL 等
	stats.mutex.Lock()
	defer stats.mutex.Unlock() // 函数退出时解锁

	color := colorGreen                                               // 默认颜色为绿色 (成功)
	status := fmt.Sprintf("成功: %v", duration.Round(time.Microsecond)) // 成功状态信息
	if err != nil {
		// 如果有错误，则为失败
		color = colorRed                    // 失败时颜色为红色
		status = fmt.Sprintf("失败: %v", err) // 失败状态信息，包含错误信息
	}

	ttlInfo := ""
	if ttl > 0 {
		// 如果 TTL 大于 0 (已获取到 TTL 值)
		ttlInfo = fmt.Sprintf(" %sTTL=%d%s", colorCyan, ttl, colorReset) // TTL 信息
	}

	if !colorMode {
		// 如果未启用彩色模式，则颜色设置为空字符串
		color = ""
		ttlInfo = fmt.Sprintf(" TTL=%d", ttl) // TTL 信息 (无颜色)
	}

	fmt.Printf("%s[#%04d] %s:%d - %s%s%s%s\n",
		color,      // 结果颜色 (绿色或红色)
		seq,        // 请求序列号
		host,       // 目标主机
		port,       // 目标端口
		status,     // 成功或失败状态信息
		ttlInfo,    // TTL 信息 (如果有)
		color,      // 尾部颜色控制符 (用于某些终端)
		colorReset) // 颜色重置
}

// getTTL 获取 TCP 连接的 TTL 值
func getTTL(conn net.Conn) (int, error) {
	// getTTL 函数尝试获取 TCP 连接的 TTL (Time-To-Live) 值
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		// TTL 功能只在 Linux 和 Darwin 系统上支持
		return 0, fmt.Errorf("不支持的操作系统")
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return 0, fmt.Errorf("非TCP连接")
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var ttl int
	var syscallErr error
	err = rawConn.Control(func(fd uintptr) {
		// 使用 syscall.GetsockoptInt 获取 IP_TTL socket 选项
		ttl, syscallErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL)
	})

	if syscallErr != nil {
		return 0, syscallErr
	}
	return ttl, err
}

// outputResults 输出最终的统计结果
func outputResults(stats *Statistics) {
	// outputResults 函数在测试结束后输出统计报告
	if stats.Total.Load() == 0 {
		return // 如果总请求数为 0，则不输出结果
	}

	c := colorCyan  // 青色
	r := colorReset // 重置颜色
	if !colorMode { // 如果未启用彩色模式，则设置为空字符串
		c, r = "", ""
	}

	fmt.Printf("\n%s--- 统计结果 ---%s\n", c, r)
	fmt.Printf("目标地址:    %s\n", stats.Target)
	fmt.Printf("总请求数:    %d\n", stats.Total.Load())
	fmt.Printf("成功数:      %d (%.2f%%)\n",
		stats.Success.Load(), // 成功请求数
		float32(stats.Success.Load())/float32(stats.Total.Load())*100) // 成功率百分比
	fmt.Printf("失败数:      %d (%.2f%%)\n",
		stats.Failed.Load(), // 失败请求数
		float32(stats.Failed.Load())/float32(stats.Total.Load())*100) // 失败率百分比

	if stats.Success.Load() > 0 {
		// 如果有成功请求，则计算并输出延迟统计信息
		avg := time.Duration(stats.Sum.Load() / int64(stats.Success.Load())) // 平均延迟
		fmt.Printf("延迟统计:    min=%-10v avg=%-10v max=%-10v\n",
			time.Duration(stats.Min.Load()).Round(time.Microsecond), // 最小延迟
			avg.Round(time.Microsecond),                             // 平均延迟
			time.Duration(stats.Max.Load()).Round(time.Microsecond)) // 最大延迟
		printLatencyDistribution(stats) // 打印延迟分布
	}

	if len(stats.TTLs) > 0 {
		// 如果有 TTL 统计数据，则打印 TTL 统计信息
		printTTLStats(stats)
	}

	if jsonOut != "" {
		// 如果指定了 JSON 输出文件路径，则保存 JSON 结果
		if err := saveJSON(stats); err != nil {
			fmt.Printf("%s保存JSON失败: %v%s\n", colorRed, err, colorReset)
		} else {
			fmt.Printf("结果已保存至: %s\n", jsonOut)
		}
	}

}

// printLatencyDistribution 打印延迟分布的直方图
func printLatencyDistribution(stats *Statistics) {
	// 使用有序切片定义延迟分布的桶范围（升序排列）
	buckets := []struct {
		name string        // 桶的名称
		max  time.Duration // 桶的最大延迟
	}{
		{"<10ms", 10 * time.Millisecond},
		{"<50ms", 50 * time.Millisecond},
		{"<100ms", 100 * time.Millisecond},
		{"<250ms", 250 * time.Millisecond}, // 新增 250ms 延迟区间
		{"<500ms", 500 * time.Millisecond},
		{"<1s", time.Second},
		{">=1s", time.Duration(math.MaxInt64)}, // 最后一个桶表示大于等于 1 秒
	}

	counts := make(map[string]int) // 存储每个桶的计数
	stats.mutex.Lock()
	defer stats.mutex.Unlock() // 函数退出时解锁

	// 遍历所有延迟，并将它们分配到对应的桶中
	for _, lat := range stats.Latencies {
		// 按升序检查每个区间
		for _, bucket := range buckets {
			if lat < bucket.max {
				counts[bucket.name]++ // 延迟属于当前桶，计数增加
				break                 // 匹配到第一个符合条件的区间后立即退出内层循环
			}
		}
	}

	fmt.Println("\n延迟分布:")
	// 打印延迟分布的直方图
	for _, bucket := range buckets {
		cnt := counts[bucket.name]
		if cnt == 0 {
			continue // 如果当前桶计数为 0，则不打印
		}
		percent := float64(cnt) / float64(len(stats.Latencies)) * 100 // 计算百分比
		color := getDistributionColor(bucket.name)                    // 获取桶对应的颜色
		fmt.Printf("%8s: %s%-5d (%.1f%%)%s\n",
			bucket.name, // 桶的名称 (例如: "<10ms")
			color,       // 桶的颜色
			cnt,         // 桶的计数
			percent,     // 桶的百分比
			colorReset)  // 颜色重置
	}
}

// getDistributionColor 根据延迟区间返回对应的颜色
func getDistributionColor(bucket string) string {
	// getDistributionColor 函数根据延迟桶的名称返回不同的颜色，用于在终端中彩色输出
	if !colorMode {
		return "" // 如果未启用彩色模式，则返回空字符串 (无颜色)
	}
	switch bucket {
	case "<10ms":
		return colorGreen // 绿色表示非常低的延迟
	case "<50ms":
		return colorCyan // 青色表示较低的延迟
	case "<100ms":
		return colorYellow // 黄色表示中等延迟
	case "<250ms": // 新增 250ms 区间
		return colorYellow // 黄色表示稍高的延迟 (与 <100ms 相同的颜色，或者您可以选择其他颜色，例如 colorCyan 或 新的颜色)
	default: // 默认情况，用于 >= 250ms 的区间
		return colorRed // 红色表示高延迟
	}
}

// printTTLStats 打印 TTL 统计信息
func printTTLStats(stats *Statistics) {
	// printTTLStats 函数打印 TTL (Time-To-Live) 值的统计信息
	stats.mutex.Lock()
	defer stats.mutex.Unlock() // 函数退出时解锁

	minTTL, maxTTL := 0, 0 // 初始化最小和最大 TTL 值
	for ttl := range stats.TTLs {
		// 遍历 TTL 统计 map，找到最小和最大 TTL 值
		if minTTL == 0 || ttl < minTTL {
			minTTL = ttl // 更新最小值
		}
		if ttl > maxTTL {
			maxTTL = ttl // 更新最大值
		}
	}

	fmt.Printf("\nTTL统计:\n")
	fmt.Printf("  范围: %d-%d\n", minTTL, maxTTL) // 打印 TTL 范围
	fmt.Printf("  分布:")
	for ttl, cnt := range stats.TTLs {
		fmt.Printf(" %d×%d", ttl, cnt) // 打印每个 TTL 值及其计数
	}
	fmt.Println()
}

// saveJSON 将统计信息保存为 JSON 文件
func saveJSON(stats *Statistics) error {
	// saveJSON 函数将 Statistics 结构体中的统计信息序列化为 JSON 格式并保存到文件
	stats.mutex.Lock()
	defer stats.mutex.Unlock() // 函数退出时解锁

	data := struct {
		Target   string        // 目标地址
		Total    int32         // 总请求数
		Success  int32         // 成功请求数
		Failed   int32         // 失败请求数
		Min      time.Duration // 最小延迟
		Max      time.Duration // 最大延迟
		Avg      time.Duration // 平均延迟
		Sum      time.Duration // 总延迟
		TTLs     map[int]int   // TTL 统计信息
		Duration time.Duration // 测试持续时间
		// Hops     []Hop `json:"hops,omitempty"` // JSON 输出 traceroute 路由信息, 移除
	}{
		Target:   stats.Target,
		Total:    stats.Total.Load(),
		Success:  stats.Success.Load(),
		Failed:   stats.Failed.Load(),
		Min:      time.Duration(stats.Min.Load()),
		Max:      time.Duration(stats.Max.Load()),
		Avg:      time.Duration(stats.Sum.Load() / int64(stats.Success.Load())),
		Sum:      time.Duration(stats.Sum.Load()),
		TTLs:     stats.TTLs,
		Duration: time.Since(stats.StartTime),
		// Hops:     stats.Hops, // 保存路由信息到 JSON, 移除
	}

	file, err := os.Create(jsonOut) // 创建 JSON 输出文件
	if err != nil {
		return err // 创建文件失败，返回错误
	}
	defer file.Close() // 确保文件关闭

	encoder := json.NewEncoder(file) // 创建 JSON 编码器
	encoder.SetIndent("", "  ")      // 设置 JSON 缩进，使其更易读
	return encoder.Encode(data)      // 编码并写入 JSON 数据到文件
}

// setupInterruptHandler 设置中断信号处理函数
func setupInterruptHandler(stats *Statistics) {
	// setupInterruptHandler 函数设置信号处理程序，监听 Ctrl+C (os.Interrupt) 和 SIGTERM 信号
	c := make(chan os.Signal, 1)                    // 创建信号通道
	signal.Notify(c, os.Interrupt, syscall.SIGTERM) // 监听中断和终止信号
	go func() {
		// 启动一个 Goroutine 来处理信号
		<-c // 阻塞等待信号

		fmt.Printf("\n%s正在终止测试...%s\n", colorYellow, colorReset) // 打印终止信息
		outputResults(stats)                                     // 输出统计结果
		os.Exit(0)                                               // 正常退出程序
	}()
}

// exitWithError 打印错误信息并退出程序
func exitWithError(msg string) {
	// exitWithError 函数打印红色错误信息并退出程序
	fmt.Printf("%s错误: %s%s\n", colorRed, msg, colorReset) // 打印红色错误信息
	os.Exit(1)                                            // 以错误码 1 退出程序
}
