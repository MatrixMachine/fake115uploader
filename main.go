package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/eiannone/keyboard"
	"github.com/valyala/fastjson"
)

// const tokenURL = "https://uplb.115.com/3.0/gettoken.php"
// const resumeURL = "https://uplb.115.com/3.0/resumeupload.php?isp=0&appid=0&appversion=%s&format=json&sig=%s"
// downloadURL   = "https://webapi.115.com/files/download?pickcode=%s"

const (
	infoURL       = "https://proapi.115.com/app/uploadinfo"
	sampleInitURL = "https://uplb.115.com/3.0/sampleinitupload.php"
	initURL       = "https://uplb.115.com/3.0/initupload.php?isp=0&appid=0&appversion=%s&format=json&sig=%s"
	getinfoURL    = "https://uplb.115.com/3.0/getuploadinfo.php"
	listFileURL   = "https://webapi.115.com/files?aid=1&cid=%d&o=user_ptime&asc=0&offset=0&show_dir=0&limit=%d&natsort=1&format=json"
	downloadURL   = "https://proapi.115.com/app/chrome/downurl"
	orderURL      = "https://webapi.115.com/files/order"
	createDirURL  = "https://webapi.115.com/files/add"
	searchURL     = "https://webapi.115.com/files/search?offset=0&limit=100000&aid=1&cid=%d&format=json"
	appVer        = "29.0.0"
	userAgent     = "Mozilla/5.0 115disk/" + appVer
	endString     = "000000"
	aliUserAgent  = "aliyun-sdk-android/2.9.1"
	linkPrefix    = "115://"
	targetPrefix  = "U_1_"
	maxParts      = 10000
)

var (
	fastUpload      *bool
	upload          *bool
	multipartUpload *bool
	hashFile        *string
	inputFile       *string
	outputFile      *string
	configFile      *string
	saveDir         *string
	internal        *bool
	removeFile      *bool
	forbidProxy     *bool
	recursive       *bool
	verbose         *bool
	userID          string
	userKey         string
	config          uploadConfig // 设置数据
	result          resultData   // 上传结果
	uploadingPart   bool
	errStopUpload   = errors.New("暂停上传")
	quit            = make(chan struct{})
	multipartCh     = make(chan struct{})
	proxyHost       string
	proxyUser       string
	proxyPassword   string
	httpClient      = &http.Client{}
)

// 设置数据
type uploadConfig struct {
	Cookies   string `json:"cookies"`   // 115网页版的Cookie
	CID       uint64 `json:"cid"`       // 115里文件夹的cid
	ResultDir string `json:"resultDir"` // 在指定文件夹保存上传结果
	HTTPRetry uint   `json:"httpRetry"` // HTTP请求失败后的重试次数
	OSSProxy  string `json:"ossProxy"`  // OSS上传代理
	PartsNum  uint   `json:"partsNum"`  // 断点续传的分片数量
}

// 上传结果数据
type resultData struct {
	Success []string `json:"success"` // 上传成功的文件
	Failed  []string `json:"failed"`  // 上传失败的文件
	Saved   []string `json:"saved"`   // 保存上传进度的文件
}

// 要上传的文件的信息
type fileInfo struct {
	Path     string `json:"path"`     // 文件路径
	ParentID uint64 `json:"parentID"` // 要上传到的文件夹的cid
}

// 检查错误
func checkErr(err error) {
	if err != nil {
		panic(err)
	}
}

// 获取时间
func getTime() string {
	t := time.Now()
	return fmt.Sprintf("%d-%02d-%02d %02d-%02d-%02d", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
}

// 处理输入
func getInput(ctx context.Context) {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("getInput() error: %v", err)
		}
	}()

	eventCh, err := keyboard.GetKeys(10)
	checkErr(err)
	defer keyboard.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-eventCh:
			checkErr(event.Err)
			if string(event.Rune) == "q" || string(event.Rune) == "Q" || event.Key == keyboard.KeyCtrlC {
				quit <- struct{}{}
				return
			}
		}
	}
}

// 退出处理
func handleQuit() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	select {
	case <-ch:
	case <-quit:
	}

	signal.Stop(ch)
	signal.Reset(os.Interrupt, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	log.Println("收到退出信号，正在退出本程序，请等待")

	if uploadingPart {
		multipartCh <- struct{}{}
		<-multipartCh
	}

	keyboard.Close()
	exitPrint()
	if len(result.Failed) != 0 {
		os.Exit(1)
	}
	os.Exit(0)
}

// 程序退出时打印信息
func exitPrint() {
	defer func() {
		if err := recover(); err != nil {
			log.Printf("exitPrint() error: %v", err)
			// 不保存上传结果
			config.ResultDir = ""
			exitPrint()
		}
	}()

	if len(result.Success) == 0 && len(result.Failed) == 0 && len(result.Saved) == 0 {
		log.Println("本次运行没有上传文件")
		return
	}

	if config.ResultDir != "" {
		resultFile := filepath.Join(config.ResultDir, getTime()+" result.json")
		log.Printf("上传结果保存在 %s", resultFile)
		data, err := json.MarshalIndent(result, "", "    ")
		checkErr(err)
		err = ioutil.WriteFile(resultFile, data, 0644)
		checkErr(err)
	}

	fmt.Printf("上传成功的文件（%d）：\n", len(result.Success))
	for _, s := range result.Success {
		fmt.Println(s)
	}
	fmt.Printf("上传失败的文件（%d）：\n", len(result.Failed))
	for _, s := range result.Failed {
		fmt.Println(s)
	}
	fmt.Printf("保存上传进度的文件（%d）：\n", len(result.Saved))
	for _, s := range result.Saved {
		fmt.Println(s)
	}
}

// 进行http请求
func doRequest(req *http.Request) (resp *http.Response, err error) {
	for i := 0; i < int(config.HTTPRetry+1); i++ {
		resp, err = httpClient.Do(req)
		if err == nil {
			return resp, nil
		} else if *verbose {
			log.Printf("http请求出现错误：%v", err)
		}
	}

	return nil, fmt.Errorf("http请求出现错误：%w", err)
}

// 获取userID和userKey
func getUserKey() (e error) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("请确定网络是否畅通或者cookies是否设置好，每一次登陆网页端115都要重设一次cookies")
			e = fmt.Errorf("getUserKey() error: %v", err)
		}
	}()

	req, err := http.NewRequest(http.MethodGet, infoURL, nil)
	checkErr(err)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", config.Cookies)
	resp, err := doRequest(req)
	checkErr(err)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	checkErr(err)

	var p fastjson.Parser
	v, err := p.ParseBytes(body)
	checkErr(err)
	userID = strconv.Itoa(v.GetInt("user_id"))
	userKey = string(v.GetStringBytes("userkey"))

	if userID == "0" {
		panic(fmt.Errorf("获取userkey出错，请确定cookies是否设置好"))
	}

	if *verbose {
		log.Printf("userID和userKey的值分别是：%s %s", userID, userKey)
	}
	return nil
}

// 在115网盘指定文件夹里创建新文件夹
func createDir(pid uint64, name string) (cid uint64, e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("createDir() error: %v", err)
		}
	}()

	form := url.Values{}
	form.Set("pid", strconv.FormatUint(pid, 10))
	form.Set("cname", name)
	v, err := postFormJSON(createDirURL, form.Encode())
	checkErr(err)

	if v.GetBool("state") {
		cid, err = strconv.ParseUint(string(v.GetStringBytes("cid")), 10, 64)
		checkErr(err)
		if *verbose {
			log.Printf("成功创建文件夹 %s ，cid：%d", name, cid)
		}
		return cid, nil
	}
	// 要创建的文件夹已经存在
	if v.GetInt("errno") == 20004 {
		reqURL, err := url.Parse(fmt.Sprintf(searchURL, pid))
		checkErr(err)
		query := reqURL.Query()
		query.Set("search_value", name)
		reqURL.RawQuery = query.Encode()
		v, err := getURLJSON(reqURL.String())
		checkErr(err)

		list := v.GetArray("data")
		for _, v := range list {
			if v.Exists("fid") {
				continue
			}
			parentID, err := strconv.ParseUint(string(v.GetStringBytes("pid")), 10, 64)
			if err != nil {
				continue
			}
			if parentID == pid && string(v.GetStringBytes("n")) == name {
				cid, err = strconv.ParseUint(string(v.GetStringBytes("cid")), 10, 64)
				checkErr(err)
				if *verbose {
					log.Printf("文件夹 %s 已存在，cid：%d", name, cid)
				}
				return cid, nil
			}
		}
	}

	return 0, fmt.Errorf("创建文件夹 %s 失败", name)
}

// 读取设置文件
func loadConfig() (e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("loadConfig() error: %v", err)
		}
	}()

	if _, err := os.Stat(*configFile); os.IsNotExist(err) {
		log.Printf("设置文件不存在，新建设置文件 %s ，请先设置cookies", *configFile)
		data, err := json.MarshalIndent(config, "", "    ")
		checkErr(err)
		err = ioutil.WriteFile(*configFile, data, 0644)
		checkErr(err)
		os.Exit(1)
	} else {
		data, err := ioutil.ReadFile(*configFile)
		checkErr(err)
		if json.Valid(data) {
			err = json.Unmarshal(data, &config)
			checkErr(err)
		} else {
			panic(fmt.Errorf("设置文件 %s 的内容不符合json格式，请检查其内容", *configFile))
		}
	}

	return nil
}

// 程序初始化
func initialize() (e error) {
	defer func() {
		if err := recover(); err != nil {
			e = fmt.Errorf("initialize() error: %v", err)
		}
	}()

	fastUpload = flag.Bool("f", false, "秒传模式上传`文件`")
	upload = flag.Bool("u", false, "先尝试用秒传模式上传`文件`，失败后改用普通模式上传")
	multipartUpload = flag.Bool("m", false, "先尝试用秒传模式上传`文件`，失败后改用断点续传模式上传，可以随时中断上传再重启上传（适合用于上传超大文件，注意暂停上传的时间不要太长）")
	hashFile = flag.String("b", "", "将指定文件的115 hashlink（115://文件名|文件大小|文件HASH值|块HASH值）追加写入到指定的`保存文件`")
	inputFile = flag.String("i", "", "从指定的`文本文件`逐行读取115 hashlink（115://文件名|文件大小|文件HASH值|块HASH值）并将其对应文件导入到115中，hashlink可以没有115://前缀")
	outputFile = flag.String("o", "", "从cid指定的115文件夹导出该文件夹内（包括子文件夹）所有文件的115 hashlink（115://文件名|文件大小|文件HASH值|块HASH值）到指定的`保存文件`")
	configFile = flag.String("l", "", "指定设置`文件`（json格式），默认是程序所在的文件夹里的fake115uploader.json")
	saveDir = flag.String("d", "", "指定存放断点续传存档文件的`文件夹`，默认是程序所在的文件夹")
	cookies := flag.String("k", "", "使用指定的115的`Cookie`")
	cid := flag.Uint64("c", 1, "上传文件到指定的115文件夹，`cid`为115里的文件夹对应的cid(默认为0，即根目录）")
	resultDir := flag.String("r", "", "将上传结果保存在指定`文件夹`")
	noConfig := flag.Bool("n", false, "不读取设置文件，需要和 -k 配合使用")
	internal = flag.Bool("a", false, "利用阿里云内网上传文件，需要在阿里云服务器上运行本程序")
	removeFile = flag.Bool("e", false, "上传成功后自动删除原文件")
	forbidProxy = flag.Bool("forbid-oss-proxy", false, "禁止使用代理上传OSS")
	ossProxy := flag.String("oss-proxy", "", "指定OSS上传使用的`代理`")
	httpRetry := flag.Uint("http-retry", 0, "HTTP请求失败后的`重试次数`，默认为0（即不重试）")
	recursive = flag.Bool("recursive", false, "递归上传文件夹")
	partsNum := flag.Uint("parts-num", 0, "断点续传模式上传文件的`分片数量`，范围为1到10000，默认为0（即自动分片）")
	verbose = flag.Bool("v", false, "显示更详细的信息（调试用）")
	help := flag.Bool("h", false, "显示帮助信息")

	flag.Parse()

	if *configFile == "" {
		path, err := os.Executable()
		checkErr(err)
		*configFile = filepath.Join(filepath.Dir(path), "fake115uploader.json")
	}

	if *saveDir == "" {
		path, err := os.Executable()
		checkErr(err)
		*saveDir = filepath.Dir(path)
	}

	if !*noConfig {
		err := loadConfig()
		checkErr(err)
	}

	if flag.NFlag() == 0 {
		log.Println("请输入正确的参数")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if *help {
		flag.PrintDefaults()
		os.Exit(0)
	}
	if (*fastUpload && *upload) || (*fastUpload && *multipartUpload) || (*upload && *multipartUpload) {
		log.Println("-f、-u和-m这三个参数只能同时使用其中一个")
		os.Exit(1)
	}

	if *partsNum != 0 && !*multipartUpload {
		log.Println("-parts-num参数只支持断点续传模式")
		os.Exit(1)
	}
	// 优先使用参数指定的分片数量
	if *partsNum != 0 {
		config.PartsNum = *partsNum
	}
	if config.PartsNum > maxParts {
		log.Printf("分片数量不能大于%d", maxParts)
		os.Exit(1)
	}

	if *hashFile != "" {
		info, err := os.Stat(*hashFile)
		if !os.IsNotExist(err) {
			if info.IsDir() {
				log.Printf("%s 不能是文件夹", *hashFile)
				os.Exit(1)
			}
		}
	}

	if *inputFile != "" {
		info, err := os.Stat(*inputFile)
		if os.IsNotExist(err) {
			log.Printf("%s 不存在", *inputFile)
			os.Exit(1)
		} else {
			if info.IsDir() {
				log.Printf("%s 不能是文件夹", *inputFile)
				os.Exit(1)
			}
		}
	}

	if *outputFile != "" {
		info, err := os.Stat(*outputFile)
		if !os.IsNotExist(err) {
			if info.IsDir() {
				log.Printf("%s 不能是文件夹", *outputFile)
				os.Exit(1)
			}
		}
	}

	// 优先使用参数指定的Cookie
	if *cookies != "" {
		config.Cookies = *cookies
	}
	if config.Cookies == "" {
		log.Printf("设置文件 %s 里的cookies不能为空字符串，或者用-k指定115的Cookie", *configFile)
		os.Exit(1)
	}
	if *verbose {
		log.Printf("Cookies的值为：%s", config.Cookies)
	}

	// 优先使用参数指定的cid
	if *cid != 1 {
		config.CID = *cid
	}

	// 优先使用参数指定的文件夹
	if *resultDir != "" {
		config.ResultDir = *resultDir
	}
	if config.ResultDir != "" {
		info, err := os.Stat(config.ResultDir)
		checkErr(err)
		if !info.IsDir() {
			log.Printf("%s 必须是文件夹，请重新设置", config.ResultDir)
			os.Exit(1)
		}
	}

	// 优先使用参数指定的HTTP请求重试次数
	if *httpRetry != 0 {
		config.HTTPRetry = *httpRetry
	}

	err := getUserKey()
	checkErr(err)

	// 将cid对应文件夹设置为时间降序
	orderBody := fmt.Sprintf("user_order=user_ptime&file_id=%d&user_asc=0&fc_mix=0", config.CID)
	v, err := postFormJSON(orderURL, orderBody)
	checkErr(err)
	if !v.GetBool("state") {
		panic(fmt.Sprintf("排序文件夹 %d 出现错误：%v", config.CID, v.GetStringBytes("error")))
	} else if *verbose {
		log.Printf("排序文件夹 %d 成功", config.CID)
	}

	// oss代理
	if !*forbidProxy {
		// 优先级 ossProxy > http_proxy > https_proxy > 设置文件
		*ossProxy = strings.TrimSpace(*ossProxy)
		if *ossProxy == "" {
			*ossProxy = strings.TrimSpace(os.Getenv("http_proxy"))
		}
		if *ossProxy == "" {
			*ossProxy = strings.TrimSpace(os.Getenv("https_proxy"))
		}
		if *ossProxy == "" {
			*ossProxy = config.OSSProxy
		}

		if *ossProxy != "" {
			proxyURL, err := url.Parse(*ossProxy)
			if err == nil {
				proxyHost = "//" + proxyURL.Host
				if proxyURL.User != nil {
					proxyUser = proxyURL.User.Username()
					if password, b := proxyURL.User.Password(); b {
						proxyPassword = password
					}
				}
			}
		}
	}

	return nil
}

func main() {
	defer func() {
		if len(result.Failed) != 0 {
			os.Exit(1)
		}
	}()

	go handleQuit()

	err := initialize()
	checkErr(err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go getInput(ctx)
	defer keyboard.Close()

	defer exitPrint()

	files := make([]fileInfo, 0, len(flag.Args()))
	cidMap := make(map[string]uint64)
	for _, file := range flag.Args() {
		info, err := os.Stat(file)
		if err != nil {
			log.Printf("获取 %s 的信息出现错误：%v", file, err)
		}
		if info.IsDir() {
			// 上传文件夹
			if *recursive {
				err = filepath.WalkDir(file, func(path string, d fs.DirEntry, err error) error {
					if d == nil {
						return fmt.Errorf("获取文件夹 %s 的信息出现错误，取消上传该文件夹：%w", path, err)
					}
					if d.IsDir() {
						if err != nil {
							log.Printf("获取文件夹 %s 的信息出现错误，取消上传该文件夹：%v", path, err)
							return fs.SkipDir
						}
						if path == file {
							var filename string
							if path == "." {
								abs, err := filepath.Abs(path)
								if err != nil {
									return fmt.Errorf("获取文件夹 %s 的绝对路径失败，取消上传该文件夹：%w", path, err)
								}
								filename = filepath.Base(abs)
							} else {
								filename = filepath.Base(path)
							}
							cid, err := createDir(config.CID, filename)
							if err != nil {
								return err
							}
							cidMap[path] = cid
							return nil
						}
						pdir := filepath.Dir(path)
						if pid, ok := cidMap[pdir]; ok {
							cid, err := createDir(pid, d.Name())
							if err != nil {
								return err
							}
							cidMap[path] = cid
						} else {
							return fmt.Errorf("没有创建文件夹 %s ，取消上传 %s", filepath.Base(pdir), path)
						}
					} else {
						if err != nil {
							log.Printf("获取文件 %s 的信息出现错误，取消上传该文件：%v", path, err)
							return nil
						}
						pdir := filepath.Dir(path)
						if pid, ok := cidMap[pdir]; ok {
							files = append(files, fileInfo{
								Path:     path,
								ParentID: pid,
							})
						} else {
							return fmt.Errorf("没有创建文件夹 %s ，取消上传 %s", filepath.Base(pdir), path)
						}
					}
					return nil
				})
				if err != nil {
					log.Printf("上传文件夹 %s 出现错误：%v", file, err)
					continue
				}
			} else {
				log.Printf("%s 是文件夹，上传文件夹需要参数 -recursive", file)
				continue
			}
		} else {
			files = append(files, fileInfo{
				Path:     file,
				ParentID: config.CID,
			})
		}
	}

	for _, file := range files {
		// 等待一秒
		time.Sleep(time.Second)
		file.uploadFile()
	}
	// 等待一秒
	time.Sleep(time.Second)

	if *hashFile != "" {
		err := write115Link()
		checkErr(err)
		log.Printf("成功将文件的115 hashlink保存在 %s", *hashFile)
	}

	if *outputFile != "" {
		err := exportHashLink()
		checkErr(err)
		log.Printf("成功将cid为 %d 的文件夹内的所有文件的115 hashlink保存在 %s", config.CID, *outputFile)
	}

	if *inputFile != "" {
		err := uploadLinkFile()
		checkErr(err)
		log.Printf("成功将 %s 里的115 hashlink导入到115", *inputFile)
	}
}

// 上传文件
func (file *fileInfo) uploadFile() {
	switch {
	case *fastUpload:
		_, err := file.fastUploadFile()
		if err != nil {
			log.Printf("秒传模式上传 %s 出现错误：%v", file.Path, err)
			result.Failed = append(result.Failed, file.Path)
			return
		}
		result.Success = append(result.Success, file.Path)
	case *upload:
		token, err := file.fastUploadFile()
		if err != nil {
			log.Printf("秒传模式上传 %s 出现错误：%v", file.Path, err)
			log.Printf("现在开始使用普通模式上传 %s", file.Path)
			err := ossUploadFile(token, file.Path)
			if err != nil {
				log.Printf("普通模式上传 %s 出现错误：%v", file.Path, err)
				result.Failed = append(result.Failed, file.Path)
				return
			}
		}
		result.Success = append(result.Success, file.Path)
	case *multipartUpload:
		// 存档文件保存在设置文件所在文件夹内
		saveFile := filepath.Join(*saveDir, filepath.Base(file.Path)+".json")
		info, err := os.Stat(saveFile)
		if os.IsNotExist(err) {
			token, err := file.fastUploadFile()
			if err != nil {
				log.Printf("秒传模式上传 %s 出现错误：%v", file.Path, err)
				log.Println("现在开始使用断点续传模式上传")
				err := multipartUploadFile(token, file.Path, nil)
				if err != nil {
					if errors.Is(err, errStopUpload) {
						return
					}
					log.Printf("断点续传模式上传 %s 出现错误：%v", file.Path, err)
					result.Failed = append(result.Failed, file.Path)
					return
				}
			}
			result.Success = append(result.Success, file.Path)
		} else {
			if info.IsDir() {
				log.Printf("%s 不能是文件夹", saveFile)
				result.Failed = append(result.Failed, file.Path)
				return
			}
			log.Printf("发现文件 %s 的上传曾经中断过，现在开始断点续传", file.Path)
			err := resumeUpload(file.Path)
			if err != nil {
				if errors.Is(err, errStopUpload) {
					return
				}
				log.Printf("断点续传模式上传 %s 出现错误：%v", file.Path, err)
				result.Failed = append(result.Failed, file.Path)
				return
			}
			result.Success = append(result.Success, file.Path)
		}
	}
}
