package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shotwise/shotwise/internal/infra/httpc"
	"github.com/shotwise/shotwise/internal/provider"
)

// 图片 backend 共享工具（端口自 lib/image_backends/base.py 的模块级函数）。
//
// 各图片 backend 复用同一套「本地图转 data URI」「从响应项落盘」逻辑，避免每家各写一份
// base64 / 下载 / 原子写盘，导致同一类降级（b64 优先、url 兜底）在不同供应商下行为分叉。

// imageMIMETypes 后缀 → MIME 类型映射（与 Python IMAGE_MIME_TYPES 同表，多个后端共用）。
var imageMIMETypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// mimeForImagePath 按后缀取 MIME，未知后缀回退 image/png（与 Python 默认一致）。
func mimeForImagePath(path string) string {
	if mime, ok := imageMIMETypes[strings.ToLower(filepath.Ext(path))]; ok {
		return mime
	}
	return "image/png"
}

// imageToBase64DataURI 把本地图片转成 base64 data URI（供 inline 传图的供应商使用）。
func imageToBase64DataURI(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read reference image %s: %w", path, err)
	}
	return "data:" + mimeForImagePath(path) + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// downloadImageToPath 从 URL 下载图片落到本地路径（先校验出站形态，再原子写盘）。
//
// 图片是「一次生成 = 一次计费」的产物，下载失败不该重跑生成；本函数只做一次下载，
// 重试由调用方按需（对比视频的 DownloadVideo 自带退避重试）。
func downloadImageToPath(ctx context.Context, client *httpc.Client, rawURL, outputPath string) error {
	validated, err := httpc.ValidateOutboundURL(rawURL)
	if err != nil {
		return err
	}
	resp, err := client.Get(ctx, validated, nil)
	if err != nil {
		return err
	}
	if len(resp.Body) == 0 {
		return fmt.Errorf("downloaded image is empty: %s", validated)
	}
	return writeFileAtomic(outputPath, resp.Body)
}

// saveImageItem 按「b64 优先、url 兜底」把响应项落盘，两者皆空报错。
func saveImageItem(ctx context.Context, client *httpc.Client, b64JSON, imageURL, outputPath string) (string, error) {
	if strings.TrimSpace(b64JSON) != "" {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64JSON))
		if err != nil {
			return "", fmt.Errorf("decode b64_json: %w", err)
		}
		if len(decoded) == 0 {
			return "", fmt.Errorf("decoded b64_json is empty")
		}
		if err := writeFileAtomic(outputPath, decoded); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	if strings.TrimSpace(imageURL) != "" {
		if err := downloadImageToPath(ctx, client, strings.TrimSpace(imageURL), outputPath); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	return "", fmt.Errorf("图片生成响应既无 b64_json 也无 url")
}

// imageCapabilitySet 便捷构造（供简单族 backend 声明能力位）。
func imageCapabilitySet(caps ...provider.ImageCapability) provider.ImageCapabilitySet {
	return provider.NewImageCapabilitySet(caps...)
}
