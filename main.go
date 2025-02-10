package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
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
	ipv6      bool
	showTTL   bool
)

type Statistics struct {
	Target    string
	Total     atomic.Int32
	Success   atomic.Int32
	Failed    atomic.Int32
	Min       atomic.Int64
	Max       atomic.Int64
	Sum       atomic.Int64
	StartTime time.Time
	Latencies []time.Duration
	TTLs      map[int]int
	mutex     sync.Mutex
}

func main() {
	// 解析命令行参数
	parseFlags()
	// 验证命令行参数的有效性
	validateArgs()

	// 构建目标地址
	target := buildTarget()
	// 初始化统计结构体
	stats := &Statistics{
		Target:    target,
		StartTime: time.Now(),
		Latencies: make([]time.Duration, 0, count),
		TTLs:      make(map[int]int),
	}
	// 设置中断处理函数
	setupInterruptHandler(stats)

	// 打印程序头部信息
	printHeader(target)

	// 创建一个通道来限制并发请求数量
	sem := make(chan struct{}, parallel)
	// 创建一个 WaitGroup 来等待所有子协程完成
	var wg sync.WaitGroup

	// 循环发送请求，直到达到指定的请求数量
	for seq := 1; count == 0 || seq <= count; {
		select {
		case sem <- struct{}{}:
			// 请求通道有空闲，开始一个新的请求
			wg.Add(1)
			go func(s int) {
				// 确保请求完成后释放通道资源
				defer func() {
					<-sem
					wg.Done()
				}()
				// 捕获并处理子协程中的 panic
				if r := recover(); r != nil {
					stats.mutex.Lock()
					fmt.Printf("%s[ERROR] panic: %v%s\n", colorRed, r, colorReset)
					stats.mutex.Unlock()
				}
				// 执行工作函数发送请求
				worker(target, stats, s)
				// 根据设定的间隔休眠
				time.Sleep(interval)
			}(seq)
			seq++
		default:
			// 请求通道已满，短暂休眠后重试
			time.Sleep(10 * time.Millisecond)
		}
	}

	// 等待所有请求完成
	wg.Wait()
	// 输出统计结果
	outputResults(stats)
}

func parseFlags() {
	// 配置选项参数
	flag.DurationVar(&timeout, "t", 2*time.Second, "连接超时时间")
	flag.IntVar(&count, "c", 0, "测试次数 (0=无限)")
	flag.DurationVar(&interval, "i", 1*time.Second, "请求间隔时间")
	flag.IntVar(&parallel, "P", 1, "并发连接数 (1-64)")
	flag.StringVar(&jsonOut, "o", "", "JSON输出文件路径")
	flag.BoolVar(&colorMode, "C", false, "启用彩色输出")
	flag.BoolVar(&ipv6, "6", false, "强制使用IPv6")
	flag.BoolVar(&showTTL, "ttl", false, "显示TTL值 (仅Unix系统)")

	// 自定义帮助信息
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使用方法: %s <目标主机> [端口] [选项]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "必选参数:")
		fmt.Fprintln(os.Stderr, "  <目标主机>\t需要测试的主机名或IP地址")
		fmt.Fprintln(os.Stderr, "可选参数:")
		fmt.Fprintln(os.Stderr, "  [端口]\t\t目标端口号 (默认为 80)")
		fmt.Fprintln(os.Stderr, "选项:")
		flag.PrintDefaults()
	}

	// 手动解析参数
	args := os.Args[1:]

	// 分离位置参数和选项参数
	var positionalArgs []string
	for i := 0; i < len(args); {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			// 如果是选项参数，直接跳过
			i++
			if f := flag.Lookup(arg[1:]); f != nil && f.Value.String() == "" {
				// 如果选项参数需要值，则跳过下一个参数
				if i < len(args) && !strings.HasPrefix(args[i], "-") {
					i++
				}
			}
		} else {
			// 如果是位置参数，提取出来并从 args 中移除
			positionalArgs = append(positionalArgs, arg)
			args = append(args[:i], args[i+1:]...)
		}
	}

	// 解析选项参数
	if err := flag.CommandLine.Parse(args); err != nil {
		exitWithError(err.Error())
	}

	// 验证位置参数
	if len(positionalArgs) < 1 {
		exitWithError("必须指定目标主机")
	}
	host = positionalArgs[0]

	// 设置端口（如果未提供，则默认为 80）
	port = 80
	if len(positionalArgs) > 1 {
		portStr := positionalArgs[1]
		var err error
		port, err = strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			exitWithError("无效的端口号 (必须为 1-65535)")
		}
	}
}

func worker(target string, stats *Statistics, seq int) {
	// 记录开始时间
	start := time.Now()
	// 设置网络类型为IPv4 TCP
	networkType := "tcp4"
	// 如果启用IPv6，则设置网络类型为IPv6 TCP
	if ipv6 {
		networkType = "tcp6"
	}

	// 尝试连接目标地址，设置超时时间
	conn, err := net.DialTimeout(networkType, target, timeout)
	// 计算连接所花费的时间
	duration := time.Since(start)

	// 增加总连接数统计
	stats.Total.Add(1)

	// 如果连接失败，增加失败连接数统计，并打印结果
	if err != nil {
		stats.Failed.Add(1)
		printResult(stats, seq, duration, err, 0)
		return
	}
	// 确保连接在函数结束时被关闭
	defer conn.Close()

	// 初始化TTL值
	ttl := 0
	// 如果需要显示TTL，则获取并更新TTL统计信息
	if showTTL {
		if t, err := getTTL(conn); err == nil {
			ttl = t
			stats.mutex.Lock()
			stats.TTLs[t]++
			stats.mutex.Unlock()
		}
	}

	// 将连接时延添加到时延列表中
	stats.mutex.Lock()
	stats.Latencies = append(stats.Latencies, duration)
	stats.mutex.Unlock()

	// 增加成功连接数统计，并累加时延
	stats.Success.Add(1)
	stats.Sum.Add(int64(duration))

	// 更新最小和最大时延
	updateMinMax(stats, duration)
	// 打印结果
	printResult(stats, seq, duration, nil, ttl)
}

func updateMinMax(stats *Statistics, d time.Duration) {
	// 循环更新最小值，直到成功或最小值不需要更新
	for {
		oldMin := stats.Min.Load()
		if oldMin == 0 || d < time.Duration(oldMin) {
			if stats.Min.CompareAndSwap(oldMin, int64(d)) {
				break
			}
		} else {
			break
		}
	}

	// 循环更新最大值，直到成功或最大值不需要更新
	for {
		oldMax := stats.Max.Load()
		if d > time.Duration(oldMax) {
			if stats.Max.CompareAndSwap(oldMax, int64(d)) {
				break
			}
		} else {
			break
		}
	}
}

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

func buildTarget() string {
	// 根据主机名是否包含端口和是否为IPv6地址，构建目标字符串
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func printHeader(target string) {
	// 打印TCPing的头部信息，包括目标地址、端口、超时时间、并发数等
	c := colorCyan
	r := colorReset
	if !colorMode {
		c, r = "", ""
	}
	fmt.Printf("%s[TCPing] 目标: %s 端口: %d 超时: %v 并发: %d%s\n",
		c, host, port, timeout, parallel, r)
}

func printResult(stats *Statistics, seq int, duration time.Duration, err error, ttl int) {
	// 打印每次TCPing的结果，包括序列号、目标地址、端口、响应状态、TTL等
	stats.mutex.Lock()
	defer stats.mutex.Unlock()

	color := colorGreen
	status := fmt.Sprintf("成功: %v", duration.Round(time.Microsecond))
	if err != nil {
		color = colorRed
		status = fmt.Sprintf("失败: %v", err)
	}

	ttlInfo := ""
	if ttl > 0 {
		ttlInfo = fmt.Sprintf(" %sTTL=%d%s", colorCyan, ttl, colorReset)
	}

	if !colorMode {
		color = ""
		ttlInfo = fmt.Sprintf(" TTL=%d", ttl)
	}

	fmt.Printf("%s[#%04d] %s:%d - %s%s%s%s\n",
		color,
		seq,
		host,
		port,
		status,
		ttlInfo,
		color,
		colorReset)
}

func getTTL(conn net.Conn) (int, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
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
		ttl, syscallErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TTL)
	})

	if syscallErr != nil {
		return 0, syscallErr
	}
	return ttl, err
}

func outputResults(stats *Statistics) {
	if stats.Total.Load() == 0 {
		return
	}

	c := colorCyan
	r := colorReset
	if !colorMode {
		c, r = "", ""
	}

	fmt.Printf("\n%s--- 统计结果 ---%s\n", c, r)
	fmt.Printf("目标地址:    %s\n", stats.Target)
	fmt.Printf("总请求数:    %d\n", stats.Total.Load())
	fmt.Printf("成功数:      %d (%.2f%%)\n",
		stats.Success.Load(),
		float32(stats.Success.Load())/float32(stats.Total.Load())*100)
	fmt.Printf("失败数:      %d (%.2f%%)\n",
		stats.Failed.Load(),
		float32(stats.Failed.Load())/float32(stats.Total.Load())*100)

	if stats.Success.Load() > 0 {
		avg := time.Duration(stats.Sum.Load() / int64(stats.Success.Load()))
		fmt.Printf("延迟统计:    min=%-10v avg=%-10v max=%-10v\n",
			time.Duration(stats.Min.Load()).Round(time.Microsecond),
			avg.Round(time.Microsecond),
			time.Duration(stats.Max.Load()).Round(time.Microsecond))
		printLatencyDistribution(stats)
	}

	if len(stats.TTLs) > 0 {
		printTTLStats(stats)
	}

	if jsonOut != "" {
		if err := saveJSON(stats); err != nil {
			fmt.Printf("%s保存JSON失败: %v%s\n", colorRed, err, colorReset)
		} else {
			fmt.Printf("结果已保存至: %s\n", jsonOut)
		}
	}
}

func printLatencyDistribution(stats *Statistics) {
	// 使用有序切片定义桶范围（升序排列）
	buckets := []struct {
		name string
		max  time.Duration
	}{
		{"<10ms", 10 * time.Millisecond},
		{"<50ms", 50 * time.Millisecond},
		{"<100ms", 100 * time.Millisecond},
		{"<500ms", 500 * time.Millisecond},
		{"<1s", time.Second},
		{">=1s", time.Duration(math.MaxInt64)},
	}

	counts := make(map[string]int)
	stats.mutex.Lock()
	defer stats.mutex.Unlock()

	for _, lat := range stats.Latencies {
		// 按升序检查每个区间
		for _, bucket := range buckets {
			if lat < bucket.max {
				counts[bucket.name]++
				break // 匹配到第一个符合条件的区间后立即退出
			}
		}
	}

	fmt.Println("\n延迟分布:")
	for _, bucket := range buckets {
		cnt := counts[bucket.name]
		if cnt == 0 {
			continue
		}
		percent := float64(cnt) / float64(len(stats.Latencies)) * 100
		color := getDistributionColor(bucket.name)
		fmt.Printf("%8s: %s%-5d (%.1f%%)%s\n",
			bucket.name,
			color,
			cnt,
			percent,
			colorReset)
	}
}

func getDistributionColor(bucket string) string {
	// 根据延迟区间返回对应的颜色
	if !colorMode {
		return ""
	}
	switch bucket {
	case "<10ms":
		return colorGreen
	case "<50ms":
		return colorCyan
	case "<100ms":
		return colorYellow
	default:
		return colorRed
	}
}

func printTTLStats(stats *Statistics) {
	// 打印TTL统计信息
	stats.mutex.Lock()
	defer stats.mutex.Unlock()

	minTTL, maxTTL := 0, 0
	for ttl := range stats.TTLs {
		if minTTL == 0 || ttl < minTTL {
			minTTL = ttl
		}
		if ttl > maxTTL {
			maxTTL = ttl
		}
	}

	fmt.Printf("\nTTL统计:\n")
	fmt.Printf("  范围: %d-%d\n", minTTL, maxTTL)
	fmt.Printf("  分布:")
	for ttl, cnt := range stats.TTLs {
		fmt.Printf(" %d×%d", ttl, cnt)
	}
	fmt.Println()
}

func saveJSON(stats *Statistics) error {
	// 将统计信息保存为JSON文件
	stats.mutex.Lock()
	defer stats.mutex.Unlock()

	data := struct {
		Target   string
		Total    int32
		Success  int32
		Failed   int32
		Min      time.Duration
		Max      time.Duration
		Avg      time.Duration
		Sum      time.Duration
		TTLs     map[int]int
		Duration time.Duration
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
	}

	file, err := os.Create(jsonOut)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}

func setupInterruptHandler(stats *Statistics) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		fmt.Printf("\n%s正在终止测试...%s\n", colorYellow, colorReset)
		outputResults(stats)
		os.Exit(0)
	}()
}

func exitWithError(msg string) {
	fmt.Printf("%s错误: %s%s\n", colorRed, msg, colorReset)
	os.Exit(1)
}
