#! /bin/bash

# install packages
install() {
    if [ $# -eq 0 ]; then
        echo "ARGS NOT FOUND"
        return 1
    fi

    for package in "$@"; do
        if ! command -v "$package" &>/dev/null; then
            if command -v dnf &>/dev/null; then
                dnf -y update && dnf install -y "$package"
            elif command -v yum &>/dev/null; then
                yum -y update && yum -y install "$package"
            elif command -v apt &>/dev/null; then
                apt update -y && apt install -y "$package"
            elif command -v apk &>/dev/null; then
                apk update && apk add "$package"
            else
                echo "UNKNOWN PACKAGE MANAGER"
                return 1
            fi
        fi
    done

    return 0
}

# 检测是否为Linux系统
if [ "$(uname)" != "Linux" ]; then
    echo "此脚本仅支持Linux系统"
    echo "Darwin/FreeBSD系统可自行拉取文件安装"
    exit 1
fi

# 检查是否为root用户
if [ "$EUID" -ne 0 ]; then
    echo "请以root用户运行此脚本"
    exit 1
fi

# 安装依赖包
install curl wget sed

# 查看当前架构是否为amd64或arm64
ARCH=$(uname -m)
if [ "$ARCH" != "x86_64" ] && [ "$ARCH" != "aarch64" ]; then
    echo " $ARCH 架构不被支持"
    exit 1
fi

# 重写架构值,改为amd64或arm64
if [ "$ARCH" == "x86_64" ]; then
    ARCH="amd64"
elif [ "$ARCH" == "aarch64" ]; then
    ARCH="arm64"
fi

tcping_dir="/usr/bin/tcping"

# 获取最新版本号
VERSION=$(curl -s https://raw.githubusercontent.com/WJQSERVER/tcping/main/VERSION)
wget -O ${ghproxy_dir}/VERSION https://raw.githubusercontent.com/WJQSERVER/tcping/main/VERSION   

# 下载最新版tcping
wget -O ${tcping_dir} https://github.com/WJQSERVER/tcping/releases/download/${VERSION}/tcping-linux-${ARCH}

# 赋予执行权限
chmod +x ${tcping_dir}

# 输出提示信息
echo "tcping安装成功"


