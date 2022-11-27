package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"runtime"

	model "github.com/cloudreve/Cloudreve/v3/models"
	"github.com/cloudreve/Cloudreve/v3/pkg/conf"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/fsctx"
	"github.com/cloudreve/Cloudreve/v3/pkg/filesystem/response"
	"github.com/cloudreve/Cloudreve/v3/pkg/thumb"
	"github.com/cloudreve/Cloudreve/v3/pkg/util"
	ffmpeg "github.com/u2takey/ffmpeg-go"
)

/* ================
     图像处理相关
   ================
*/

// HandledImageExtension 可以生成缩略图的文件扩展名
var HandledImageExtension = []string{"jpg", "jpeg", "png", "gif"}

// HandledVideoExtension 可以生成缩略图的文件扩展名
var HandledVideoExtension = []string{"mp4", "mkv", "webm", "avi"}

// GetThumb 获取文件的缩略图
func (fs *FileSystem) GetThumb(ctx context.Context, id uint) (*response.ContentResponse, error) {
	// 根据 ID 查找文件
	err := fs.resetFileIDIfNotExist(ctx, id)
	if err != nil || fs.FileTarget[0].PicInfo == "" {
		return &response.ContentResponse{
			Redirect: false,
		}, ErrObjectNotExist
	}

	w, h := fs.GenerateThumbnailSize(0, 0)
	ctx = context.WithValue(ctx, fsctx.ThumbSizeCtx, [2]uint{w, h})
	ctx = context.WithValue(ctx, fsctx.FileModelCtx, fs.FileTarget[0])
	res, err := fs.Handler.Thumb(ctx, fs.FileTarget[0].SourceName)

	// 本地存储策略出错时重新生成缩略图
	if err != nil && fs.Policy.Type == "local" {
		fs.GenerateThumbnail(ctx, &fs.FileTarget[0])
		res, err = fs.Handler.Thumb(ctx, fs.FileTarget[0].SourceName)
	}

	if err == nil && conf.SystemConfig.Mode == "master" {
		res.MaxAge = model.GetIntSetting("preview_timeout", 60)
	}

	return res, err
}

// thumbPool 要使用的任务池
var thumbPool *ThumbPool
var thumbPoolOnce sync.Once

// ThumbPool 带有最大配额的任务池
type ThumbPool struct {
	// 容量
	worker chan int
}

// Init 初始化任务池
func getThumbWorker() *ThumbPool {
	thumbPoolOnce.Do(func() {
		maxWorker := model.GetIntSetting("thumb_max_task_count", -1)
		if maxWorker <= 0 {
			maxWorker = runtime.GOMAXPROCS(0)
		}
		thumbPool = &ThumbPool{
			worker: make(chan int, maxWorker),
		}
		util.Log().Debug("Initialize thumbnails task queue with: WorkerNum = %d", maxWorker)
	})
	return thumbPool
}
func (pool *ThumbPool) addWorker() {
	pool.worker <- 1
	util.Log().Debug("Worker added to thumbnails task queue.")
}
func (pool *ThumbPool) releaseWorker() {
	util.Log().Debug("Worker released from thumbnails task queue.")
	<-pool.worker
}

func (fs *FileSystem) GeneratePreviewImage(ctx context.Context, file *model.File) {
	sourceName := file.SourceName
	previewName := sourceName + model.GetSettingByNameWithDefault("preview_file_suffix", "._preview.jpeg")

	// 判断是否可以生成预览图
	if !IsInExtensionList(HandledVideoExtension, sourceName) || file.Size == 0 {
		return
	}

	getThumbWorker().addWorker()
	defer getThumbWorker().releaseWorker()

	buf := bytes.NewBuffer(nil)
	err := ffmpeg.Input(util.RelativePath(sourceName)).Filter("select", ffmpeg.Args{fmt.Sprintf("gte(n,%d)", 0)}).
		Output("pipe:", ffmpeg.KwArgs{"vframes": 1, "format": "image2", "vcodec": "mjpeg"}).
		WithOutput(buf, io.Discard).
		Run()
	if err != nil {
		util.Log().Warning("Cannot generate preview image because of failed to parse video %q: %s", sourceName, err)
		return
	}

	image, err := os.Create(util.RelativePath(previewName))
	if err != nil {
		util.Log().Warning("Failed to save preview image %s", err)
		return
	}
	defer image.Close()

	n, err := image.WriteAt(buf.Bytes(), 0)
	if err != nil {
		util.Log().Warning("Failed to save preview image %s", err)
		return
	}
	if n != buf.Len() {
		util.Log().Warning("Failed to save preview image %s", err)
		return
	}
}

// GenerateThumbnail 尝试为本地策略文件生成缩略图并获取图像原始大小
// TODO 失败时，如果之前还有图像信息，则清除
func (fs *FileSystem) GenerateThumbnail(ctx context.Context, file *model.File) {
	sourceName := file.SourceName
	previewName := ""
	thumbName := sourceName + model.GetSettingByNameWithDefault("thumb_file_suffix", "._thumb")

	// 判断是否可以生成预览图
	if IsInExtensionList(HandledVideoExtension, sourceName) {
		fs.GeneratePreviewImage(ctx, file)
		previewName = sourceName + model.GetSettingByNameWithDefault("preview_file_suffix", "._preview.jpeg")
		sourceName = previewName
	}

	// 判断是否可以生成缩略图
	if len(previewName) == 0 && !IsInExtensionList(HandledImageExtension, sourceName) {
		return
	}

	// 新建上下文
	newCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 获取文件数据
	source, err := fs.Handler.Get(newCtx, sourceName)
	if err != nil {
		return
	}
	defer source.Close()
	getThumbWorker().addWorker()
	defer getThumbWorker().releaseWorker()

	image, err := thumb.NewThumbFromFile(source, sourceName)
	if err != nil {
		util.Log().Warning("Cannot generate thumb because of failed to parse image %q: %s", sourceName, err)
		return
	}

	// 获取原始图像尺寸
	w, h := image.GetSize()

	// 生成缩略图
	image.GetThumb(fs.GenerateThumbnailSize(w, h))
	// 保存到文件
	err = image.Save(util.RelativePath(thumbName))
	image = nil
	if model.IsTrueVal(model.GetSettingByName("thumb_gc_after_gen")) {
		util.Log().Debug("GenerateThumbnail runtime.GC")
		runtime.GC()
	}

	if err != nil {
		util.Log().Warning("Failed to save thumb: %s", err)
		return
	}

	// 更新文件的图像信息
	if file.Model.ID > 0 {
		err = file.UpdatePicInfo(fmt.Sprintf("%d,%d", w, h))
	} else {
		file.PicInfo = fmt.Sprintf("%d,%d", w, h)
	}

	// 失败时删除缩略图文件
	if err != nil {
		_, _ = fs.Handler.Delete(newCtx, []string{thumbName})
	}
}

// GenerateThumbnailSize 获取要生成的缩略图的尺寸
func (fs *FileSystem) GenerateThumbnailSize(w, h int) (uint, uint) {
	return uint(model.GetIntSetting("thumb_width", 400)), uint(model.GetIntSetting("thumb_width", 300))
}
