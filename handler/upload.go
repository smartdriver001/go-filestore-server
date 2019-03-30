package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"go-filestore-server/common"
	"go-filestore-server/config"
	"go-filestore-server/db"
	"go-filestore-server/meta"
	"go-filestore-server/mq"
	"go-filestore-server/store/ceph"
	"go-filestore-server/store/oss"
	"go-filestore-server/util"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func init() {
	// 目录已存在
	if _, err := os.Stat(config.TempLocalRootDir); err == nil {
		return
	}

	// 尝试创建目录
	err := os.MkdirAll(config.TempLocalRootDir, 0744)
	if err != nil {
		log.Println("无法创建临时存储目录，程序将退出")
		os.Exit(1)
	}
}

/*
// 处理文件上传
func UploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		// 返回上传html页面
		data, err := ioutil.ReadFile("./static/view/index.html")
		if err != nil {
			io.WriteString(w, "internel server error")
			return
		}
		io.WriteString(w, string(data))
		// 另一种返回方式:
		// 动态文件使用http.HandleFunc设置，静态文件使用http.FileServer设置
		// 所有直接redirect到http.FileServer所配置的url
		// http.Redirect(w,r,"./static/view/index.html",http.StatusFound)
	} else if r.Method == "POST" {
		// 1.从form表单中获得文件句柄
		file, head, err := r.FormFile("file")
		if err != nil {
			fmt.Println("failed to get form data, err:\t", err.Error())
		}
		defer file.Close()

		// 2.把文件内容转为[]byte
		buf := bytes.NewBuffer(nil)
		if _, err := io.Copy(buf, file); err != nil {
			fmt.Println("failed to get file data, err:\t", err.Error())
			return
		}

		// 3.构建文件元信息
		fileMeta := meta.FileMeta{
			FileName: head.Filename,
			FileHash: util.Sha1(buf.Bytes()), // 计算文件sha1
			FileSize: int64(len(buf.Bytes())),
			UploadAt: time.Now().Format(common.StandardTimeFormat),
		}

		// 4.将文件写入临时存储位置
		fileMeta.Location = config.TempLocalRootDir + fileMeta.FileHash
		newFile, err := os.Create(fileMeta.Location)
		if err != nil {
			fmt.Println("failed to create file, err:\t", err.Error())
			return
		}
		defer newFile.Close()

		nByte, err := newFile.Write(buf.Bytes())
		if int64(nByte) != fileMeta.FileSize || err != nil {
			fmt.Println("failed to sava data into file, writtenSize:\t", nByte, ",err:\t", err.Error())
			return
		}

		// 5.同步或异步将文件转移到Ceph/OSS
		newFile.Seek(0, 0) // 游标重新回到文件头部
		if config.CurrentStoreType == common.StoreCeph {
			// 文件写入Ceph存储
			data, _ := ioutil.ReadAll(newFile)
			cephPath := "/ceph/" + fileMeta.FileHash
			_ = ceph.PutObject("userfile", cephPath, data)
			fileMeta.Location = cephPath
		} else if config.CurrentStoreType == common.StoreOSS {
			// 文件写入到OSS存储
			ossPath := "oss/" + fileMeta.FileHash
			// 判断写入OSS为同步还是异步
			if !config.AsyncTransferEnable {
				err = oss.Bucket().PutObject(ossPath, newFile)
				if err != nil {
					fmt.Println(err.Error())
					w.Write([]byte("upload failed!"))
					return
				}
				fileMeta.Location = ossPath
			} else {
				// 写入异步转移任务队列
				data := mq.TransferData{
					FileHash:      fileMeta.FileHash,
					CurLocation:   fileMeta.Location,
					DestLocation:  ossPath,
					DestStoreType: common.StoreOSS,
				}
				pubData, _ := json.Marshal(data)
				pubSuc := mq.Publish(
					config.TransExchangeName,
					config.TransOSSRoutingKey,
					pubData)
				if !pubSuc {
					// TODO: 当前发送转移信息失败，稍后重试
				}
			}
		}

		// 6. 更新用户文件表记录
		_ = meta.UpdateFileMetaDB(fileMeta)

		r.ParseForm()
		username := r.Form.Get("username")
		suc := db.OnUserFileUploadFinished(username, fileMeta.FileHash, fileMeta.FileName, fileMeta.FileSize)
		if suc {
			http.Redirect(w, r, "/static/view/home.html", http.StatusFound)
		} else {
			w.Write([]byte("upload failed."))
		}
	}
}

// 上传已完成
func UploadSucHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "Upload finished!")
}

// 获取文件元信息
func GetFileMetaHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	filehash := r.Form["filehash"][0]
	fMeta, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if fMeta != nil {
		data, err := json.Marshal(fMeta)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write(data)
	} else {
		w.Write([]byte(`{"code":-1,"msg":"no such file"}`))
	}
}

// 批量查询文件元信息
func FileQueryHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	limitCnt, _ := strconv.Atoi(r.Form.Get("limit"))
	username := r.Form.Get("username")

	userFiles, err := db.QueryUserFileMetas(username, limitCnt)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	data, err := json.Marshal(userFiles)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(data)
}

// 文件下载接口
func DownloadHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	filehash := r.Form.Get("filehash")
	username := r.Form.Get("username")

	fmeta, _ := meta.GetFileMetaDB(filehash)
	userFile, err := db.QueryUserFileMeta(username, filehash)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	f, err := os.Open(fmeta.Location)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octect-stream")
	// attachment表示文件将会提示下载到本地，而不是直接在浏览器中打开
	w.Header().Set("content-disposition", "attachment; filename=\""+userFile.FileName+"\"")
	w.Write(data)
}

// 更新元信息接口
func FileMetaUpdateHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	opType := r.Form.Get("op")
	filehash := r.Form.Get("filehash")
	username := r.Form.Get("username")
	newFileName := r.Form.Get("filename")

	if opType != "0" || len(newFileName) < 1 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// 更新用户文件表tbl_user_file中的文件名，tbl_file的文件名不用修改
	_ = db.RenameFileName(username, filehash, newFileName)

	// 返回最新的文件信息
	userFile, err := db.QueryUserFileMeta(username, filehash)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	data, err := json.Marshal(userFile)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// 删除文件及元信息
func FileDeleteHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	username := r.Form.Get("username")
	filehash := r.Form.Get("filehash")
	fmeta, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// 删除本地文件
	os.Remove(fmeta.Location)
	// TODO: 可以考虑删除Ceph/OSS上的文件
	// 可以不立即删除，加个超时机制
	// 比如该文件10天后也没有用户再次上传，那么就可以真正的删除了

	// 删除文件表中的一条记录
	suc := db.DeleteUserFile(username, filehash)
	if !suc {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// 尝试秒传接口
func TryFastUploadHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()

	// 1.解析请求参数
	username := r.Form.Get("username")
	filehash := r.Form.Get("filehash")
	filename := r.Form.Get("filename")
	filesize, _ := strconv.Atoi(r.Form.Get("filesize"))

	// 2.从文件表中查询相同hash的文件记录
	fileMeta, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// 3.差不多记录则返回秒传失败
	if fileMeta == nil {
		resp := util.RespMsg{
			Code: -1,
			Msg:  "秒传失败,请访问普通上传接口",
		}
		w.Write(resp.JSONBytes())
		return
	}

	// 4.上传过则将文件信息写入用户文件表，返回成功
	suc := db.OnUserFileUploadFinished(username, filehash, filename, int64(filesize))
	if suc {
		resp := util.RespMsg{
			Code: 0,
			Msg:  "秒传成功",
		}
		w.Write(resp.JSONBytes())
		return
	}
	resp := util.RespMsg{
		Code: -2,
		Msg:  "秒传失败，请稍后重试",
	}
	w.Write(resp.JSONBytes())
	return
}

// 生成文件的下载地址
func DownloadURLHandler(w http.ResponseWriter, r *http.Request) {
	filehash := r.Form.Get("filehash")
	// 从文件表查找记录
	row, _ := db.GetFileMeta(filehash)

	// TODO: 判断文件存在OSS,还是Ceph，还是在本地
	if strings.HasPrefix(row.FileAddr.String, config.TempLocalRootDir) {
		username := r.Form.Get("username")
		token := r.Form.Get("token")
		tmpUrl := fmt.Sprintf("http://%s/file/download?filehash=%s&username=%s&token=%s", r.Host, filehash, username, token)
		w.Write([]byte(tmpUrl))
	} else if strings.HasPrefix(row.FileAddr.String, "/ceph") {
		// TODO: ceph下载url
	} else if strings.HasPrefix(row.FileAddr.String, "oss/") {
		// oss下载url
		signedURL := oss.DownloadURL(row.FileAddr.String)
		w.Write([]byte(signedURL))
	}
}
*/

// Gin版本
// 响应上传页面
func UploadHandler(c *gin.Context) {
	data, err := ioutil.ReadFile("./static/view/upload.html")
	if err != nil {
		c.String(404, `网页不存在`)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

// 处理文件上传
func DoUploadHandler(c *gin.Context) {
	errCode := 0
	defer func() {
		if errCode < 0 {
			c.JSON(http.StatusOK, gin.H{
				"code": errCode,
				"msg":  "upload failed",
			})
		}
	}()

	// 1.从form表单中获得文件内容句柄
	file, head, err := c.Request.FormFile("file")
	if err != nil {
		fmt.Printf("failed to get form data, err:%s\n", err.Error())
		errCode = -1
		return
	}
	defer file.Close()

	// 2.把文件内容转为[]byte
	buf := bytes.NewBuffer(nil)
	if _, err := io.Copy(buf, file); err != nil {
		fmt.Printf("failed to get file data,err:%s\n", err.Error())
		errCode = -2
		return
	}

	// 3.构建文件元信息
	fileMeta := meta.FileMeta{
		FileName: head.Filename,
		FileHash: util.Sha1(buf.Bytes()),
		FileSize: int64(len(buf.Bytes())),
		UploadAt: time.Now().Format(common.StandardTimeFormat),
	}

	// 4.将文件写入临时存储位置
	fileMeta.Location = config.TempLocalRootDir + fileMeta.FileHash
	newFile, err := os.Create(fileMeta.Location)
	if err != nil {
		fmt.Printf("failed to create file, err:%s\n", err.Error())
		errCode = -3
		return
	}
	defer newFile.Close()

	nByte, err := newFile.Write(buf.Bytes())
	if int64(nByte) != fileMeta.FileSize || err != nil {
		fmt.Printf("failed to save data into file, writtenSize:%d,err:%s\n", nByte, err.Error())
		errCode = -4
		return
	}

	// 5.同步或异步将文件转移到Ceph/OSS
	newFile.Seek(0, 0) // 游标重新回到文件头部
	if config.CurrentStoreType == common.StoreCeph {
		// 文件写入Ceph存储
		data, _ := ioutil.ReadAll(newFile)
		cephPath := "/ceph/" + fileMeta.FileHash
		_ = ceph.PutObject("userfile", cephPath, data)
		fileMeta.Location = cephPath
	} else if config.CurrentStoreType == common.StoreOSS {
		// 文件写入OSS存储
		ossPath := "oss/" + fileMeta.FileHash
		// 判断写入OSS为同步还是异步
		if !config.AsyncTransferEnable {
			// TODO 设置oss的文件名， 方便指定文件名下载
			err = oss.Bucket().PutObject(ossPath, newFile)
			if err != nil {
				fmt.Println(err.Error())
				errCode = -5
				return
			}
			fileMeta.Location = ossPath
		} else {
			// 写入异步转移任务队列
			data := mq.TransferData{
				FileHash:      fileMeta.FileHash,
				CurLocation:   fileMeta.Location,
				DestStoreType: common.StoreOSS,
				DestLocation:  ossPath,
			}
			pubData, _ := json.Marshal(data)
			pubSuc := mq.Publish(config.TransExchangeName,
				config.TransOSSRoutingKey,
				pubData)
			if !pubSuc {
				// TODO 当前发送转移信息失败，稍后重试
			}
		}
	}

	// 6.更新文件表记录
	_ = meta.UpdateFileMetaDB(fileMeta)

	// 7.更新用户文件表
	username := c.Request.FormValue("username")
	suc := db.OnUserFileUploadFinished(username, fileMeta.FileHash, fileMeta.FileName, fileMeta.FileSize)
	if suc {
		c.Redirect(http.StatusFound, "/static/view/home.html")
	} else {
		errCode = -6
	}
}

// 上传已经完成
func UploadSucHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"msg":  "upload finish",
	})
	return
}

// 获取文件元信息
func GetFileMetaHandler(c *gin.Context) {
	filehash := c.Request.FormValue("filehash")
	fMeta, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": -2,
			"msg":  "upload failed!",
		})
		return
	}

	if fMeta != nil {
		data, err := json.Marshal(fMeta)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"code": -3,
				"msg":  "upload failed!",
			})
			return
		}
		c.Data(http.StatusOK, "application/json", data)
	} else {
		c.JSON(http.StatusOK, gin.H{
			"code": -4,
			"msg":  "no such file",
		})
	}
}

// 批量查询文件元信息
func FileQueryHandler(c *gin.Context) {
	limitCnt, _ := strconv.Atoi(c.Request.FormValue("limit"))
	username := c.Request.FormValue("username")
	userFiles, err := db.QueryUserFileMetas(username, limitCnt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": -1,
			"msg":  "query failed!",
		})
		return
	}
	data, err := json.Marshal(userFiles)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": -2,
			"msg":  "query failed!",
		})
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

// 文件下载接口
func DownloadHandler(c *gin.Context) {
	filehash := c.Request.FormValue("filehash")
	username := c.Request.FormValue("username")
	// TODO: 处理异常情况
	fm, _ := meta.GetFileMetaDB(filehash)
	userFile, _ := db.QueryUserFileMeta(username, filehash)

	if strings.HasPrefix(fm.Location, config.TempLocalRootDir) {
		c.FileAttachment(fm.Location, userFile.FileName)
	} else if strings.HasPrefix(fm.Location, config.CephRootDir) {
		// ceph中的文件，通过ceph api先下载
		bucket := ceph.GetCephBucket("userfile")
		data, _ := bucket.Get(fm.Location)
		c.Header("content-disposition", "attachment;filename=\""+userFile.FileName+"\"")
		c.Data(http.StatusOK, "application/octect-stream", data)
	}
}

// 更新元信息接口
func FileMetaUpdateHandler(c *gin.Context) {
	opType := c.Request.FormValue("op")
	filehash := c.Request.FormValue("filehash")
	username := c.Request.FormValue("username")
	newFileName := c.Request.FormValue("filename")

	if opType != "0" || len(newFileName) < 1 {
		c.Status(http.StatusForbidden)
		return
	}

	// 更新用户文件表tbl_user_file中的文件名，tbl_file的文件名不用修改
	_ = db.RenameFileName(username, filehash, newFileName)

	// 返回最新的文件信息
	userFile, err := db.QueryUserFileMeta(username, filehash)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	data, err := json.Marshal(userFile)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.JSON(http.StatusOK, data)
}

// 删除文件及元信息
func FileDeleteHandler(c *gin.Context) {
	username := c.Request.FormValue("username")
	filehash := c.Request.FormValue("filehash")
	fm, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}

	// 删除本地文件
	os.Remove(fm.Location)
	// TODO: 可以考虑删除Ceph/OSS上的文件
	// 可以不立即删除，加个超时机制
	// 比如该文件10天后也没有用户再次上传，那么就可以真正的删除了

	// 删除文件表中的一条记录
	suc := db.DeleteUserFile(username, filehash)
	if !suc {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusOK)
}

// 尝试秒传接口
func TryFastUploadHandler(c *gin.Context) {
	// 1.解析请求参数
	username := c.Request.FormValue("username")
	filehash := c.Request.FormValue("filehash")
	filename := c.Request.FormValue("filename")
	filesize, _ := strconv.Atoi(c.Request.FormValue("filesize"))

	// 2.从文件表中查询相同hash的文件记录
	fileMeta, err := meta.GetFileMetaDB(filehash)
	if err != nil {
		fmt.Println(err.Error())
		c.Status(http.StatusInternalServerError)
		return
	}

	// 3.查不到记录则返回秒传失败
	if fileMeta == nil {
		resp := util.RespMsg{
			Code: -1,
			Msg:  "秒传失败，请访问普通上传接口",
		}
		c.Data(http.StatusOK, "application/json", resp.JSONBytes())
		return
	}

	// 4.上传过则将文件信息写入到用户表，返回成功
	suc := db.OnUserFileUploadFinished(username, filehash, filename, int64(filesize))
	if suc {
		resp := util.RespMsg{
			Code: 0,
			Msg:  "秒传成功",
		}
		c.Data(http.StatusOK, "application/json", resp.JSONBytes())
		return
	}
	resp := util.RespMsg{
		Code: -2,
		Msg:  "秒传失败，请稍后重试",
	}
	c.Data(http.StatusOK, "application/json", resp.JSONBytes())
	return
}

// 生成文件的下载地址
func DownloadURLHandler(c *gin.Context) {
	filehash := c.Request.FormValue("filehash")
	// 从文件表查找记录
	row, _ := db.GetFileMeta(filehash)

	// TODO 判断文件存储在OSS，还是Ceph，还是在本地
	if strings.HasPrefix(row.FileAddr.String, config.TempLocalRootDir) ||
		strings.HasPrefix(row.FileAddr.String, config.CephRootDir) {
		username := c.Request.FormValue("username")
		token := c.Request.FormValue("token")
		tmpURL := fmt.Sprintf("http://%s/file/download?filehash=%s&username=%s&token=%s",
			c.Request.Host, filehash, username, token)
		c.Data(http.StatusOK, "octet-stream", []byte(tmpURL))
	} else if strings.HasPrefix(row.FileAddr.String, "oss/") {
		// oss 下载url
		signedURL := oss.DownloadURL(row.FileAddr.String)
		fmt.Println(row.FileAddr.String)
		c.Data(http.StatusOK, "octet-stream", []byte(signedURL))
	}
}
