# tcping

Go实现的简单tcping

## 使用

```
使用方法: tcping <目标主机> [端口] [选项]

必选参数:
  <目标主机>    需要测试的主机名或IP地址
可选参数:
  [端口]                目标端口号 (默认为 80)
选项:
  -C    启用彩色输出
  -P int
        并发连接数 (1-64) (default 1)
  -c int
        测试次数 (0=无限)
  -i duration
        请求间隔时间 (default 1s)
  -o string
        JSON输出文件路径
  -t duration
        连接超时时间 (default 2s)
  -ttl
        显示TTL值 (仅Unix系统)
```

## 安装

一键安装脚本：

```bash
wget -O tcping.sh https://raw.githubusercontent.com/WJQSERVER/tcping/main/install.sh && chmod +x tcping.sh &&./tcping.sh
```

## 演示

![tcping演示](https://raw.githubusercontent.com/WJQSERVER/tcping/main/example.png)
