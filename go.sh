#!/bin/sh

# 检测系统架构
ARCH=$(uname -m)
CFNAT_BINARY='./cfnat'
colo_upper=$(echo "$colo" | tr '[:lower:]' '[:upper:]')

# 记录启动信息
{
echo "系统架构: $ARCH"
echo "使用二进制文件: $CFNAT_BINARY"
echo "IP类型(ips): $ips"
echo "TLS: $tls"
echo "随机IP(random): $random"
echo "数据中心(colo): $colo_upper"
echo "有效延迟(delay): $delay"
echo "转发端口(port): $port"
echo "负载IP数(num): $num"
echo "池大小(ipnum): $ipnum"
echo "探测间隔(probe): $probe"
echo "探测次数(proben): ${proben:-3}"
echo "丢包阈值(loss): ${loss:-20}%"
echo "最大并发请求数(task): $task"
echo "响应状态码(code): $code"
echo "检查域名(domain): $domain"
} >> cfnat.log

while true
do
    # 记录每次启动时间
    echo "$(date '+%Y-%m-%d %H:%M:%S') - cfnat 启动 ..." >> cfnat.log
    
    # 运行主程序 - 使用检测到的正确二进制文件
    $CFNAT_BINARY \
        -colo="$colo_upper" \
        -port="$port" \
        -delay="$delay" \
        -ips="$ips" \
        -addr="0.0.0.0:1234" \
        -ipnum="$ipnum" \
        -num="$num" \
        -probe="$probe" \
        -proben="${proben:-3}" \
        -loss="${loss:-20}" \
        -random="$random" \
        -task="$task" \
        -tls="$tls" \
        -code="$code" \
        -domain="$domain"

    # 检查执行是否成功
    if [ $? -ne 0 ]; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') - cfnat 崩溃，5 秒后重启..." >> cfnat.log
    fi

    # 等待 5 秒后重启
    sleep 5
done