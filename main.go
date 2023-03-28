package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gin-gonic/gin"
	"github.com/russross/blackfriday/v2"
)

// 这里填写你的openai api key
var GPTAPIKey = "sk-LVaufkQojm70c3Cht8T9T3BlbkFJdMXXXXXXXXXXXXXXXzMK"

// 这里填写群号
var GroupID = 1101101101

// 冷却时间, 单位秒
var ColdDownTime = 60

// 所有Event的公用字段
type EventHeaderStruct struct {
	// unix事件戳
	Time int64 `json:"time"`
	// message, message_sent, request, notice, meta_event
	PostType string `json:"post_type"`
	// 自身的QQ号
	SelfID int64 `json:"self_id"`
}

type sender struct {
	// 私聊有下面四个字段

	// QQ号
	UserID int64 `json:"user_id"`
	// 昵称
	NickName string `json:"nickname"`
	// "male", "female", "unknown"
	Sex string `json:"sex"`
	// 年龄
	Age int32 `json:"age"`

	// 群临时会话除了上面的字段, 还有下面的字段
	GroupID int64 `json:"group_id"`

	// 如果是群聊， 除了上面四个基础字段, 还有下面的字段

	// 群名片/备注
	Card string `json:"card"`
	// 地址
	Area string `json:"area"`
	// 成员等级
	Level string `json:"level"`
	// 成员角色, owner, admin, member
	Role string `json:"role"`
	// 头衔
	Title string `json:"title"`
}

type EventMessageStrcut struct {
	EventHeaderStruct
	MessageType string `json:"message_type"` // 消息类型, 私聊, 群聊 private, group
	SubType     string `json:"sub_type"`     // 消息的子类型: group, public, friend, normal
	MessageID   int32  `json:"message_id"`   // 消息号
	RawMessage  string `json:"raw_message"`  // 原始消息, 带有CQ码
	Sender      sender `json:"sender"`
	GroupID     int64  `json:"group_id"` // 对于群聊消息, 才有群号
	// 注意禁用匿名消息, 为了简便起见
}

type SendMsgStruct struct {
	// 消息类型, private, group
	MessageType string `json:"message_type"`
	// 仅私聊时需要
	UserID int64 `json:"user_id"`
	// 仅群聊时需要
	GroupID int64  `json:"group_id"`
	Message string `json:"message"`
	// 是否作为纯文本
	AutoEscape bool `json:"auto_escape"`
}

type gptMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type GPTRequestStruct struct {
	Model       string       `json:"model"`
	Messages    []gptMessage `json:"messages"`
	Temperature float64      `json:"temperature"`
	N           int          `json:"n"`
}

type choice struct {
	Msg gptMessage `json:"message"`
}

type GPTResponseStruct struct {
	Choices []choice `json:"choices"`
}

// 冷却时间支持
var WaitTable = map[int64]*time.Time{}

// 用条件变量来维护队列
var TaskQueue []Task
var TaskQueueMutex sync.Mutex
var TaskCond *sync.Cond = sync.NewCond(&TaskQueueMutex)

type Task struct {
	TargetQQ int64
	Question string
	GroupID  int64
	Nickname string
	NextUse  time.Time
}

var Render = `<link href="./markdown.css" rel="stylesheet"></link>
%s`

func fullScreenshot(urlstr string, quality int, res *[]byte) chromedp.Tasks {
	return chromedp.Tasks{
		chromedp.Navigate(urlstr),
		chromedp.FullScreenshot(res, quality),
	}
}

func Sender() {
	var gotTasks []Task
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	chromePath := fmt.Sprintf("file://%s/tmp.html", wd)

	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()

	println(chromePath)

	for {
		if len(gotTasks) != 0 {
			// 处理所有的消息
			for _, v := range gotTasks {
				// 发请求
				gptmsgs := []gptMessage{}
				gptmsgs = append(gptmsgs, gptMessage{Role: "system", Content: "你是一个得力的编程助手"}, gptMessage{Role: "user", Content: v.Question})
				gptReqStruct := GPTRequestStruct{
					Model:       "gpt-3.5-turbo",
					Messages:    gptmsgs,
					N:           1,
					Temperature: 0.8,
				}
				gptReqBytes, _ := json.Marshal(gptReqStruct)
				gptRequest, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(gptReqBytes))
				gptRequest.Header.Add("Content-Type", "application/json")
				gptRequest.Header.Add("Authorization", "Bearer "+GPTAPIKey)
				resp, err := http.DefaultClient.Do(gptRequest)
				if err != nil {
					log.Printf("failed to Post to GPT server: %v\n", err)
					responseToUser := err.Error()
					resp, err = http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, v.GroupID, v.TargetQQ)))
					if err != nil {
						log.Printf("failed to post to api: %v\n", err)
						continue
					}
					resp.Body.Close()
					continue
				}
				if resp.StatusCode != 200 {
					log.Printf("failed to Post to GPT server: code is not 200\n")
					responseToUser := "failed to Post to GPT server: code is not 200"
					resp, err = http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, v.GroupID, v.TargetQQ)))
					if err != nil {
						log.Printf("failed to post to api: %v\n", err)
						continue
					}
					resp.Body.Close()
					continue
				}

				// 拿到resp.Body的所有响应
				gptRespBytes, err := io.ReadAll(resp.Body)
				if err != nil {
					// 如果失败就发送给用户一些信息
					log.Printf("failed to Readall from GPT response: %v\n", err)
					responseToUser := err.Error()
					resp, err := http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, v.GroupID, v.TargetQQ)))
					if err != nil {
						log.Printf("failed to post to api: %v\n", err)
						continue
					}
					resp.Body.Close()
					continue
				}
				fmt.Println(string(gptRespBytes))

				// 解析出结构体
				gptRespStruct := GPTResponseStruct{}
				if err := json.Unmarshal(gptRespBytes, &gptRespStruct); err != nil {
					// 如果失败就发送给用户一些信息
					log.Printf("failed to Unmarsh JSON from GPT response: %v\n", err)
					responseToUser := err.Error()
					resp, err := http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, v.GroupID, v.TargetQQ)))
					if err != nil {
						log.Printf("failed to post to api: %v\n", err)
						continue
					}
					resp.Body.Close()
					continue
				}

				responseToUser := gptRespStruct.Choices[0].Msg.Content

				output := string(blackfriday.Run([]byte(responseToUser + fmt.Sprintf("\n\n---\n\n提问者: %v(%v)\n\n问题: %v\n\n你的下次使用时间在%v之后", v.Nickname, v.TargetQQ, v.Question, v.NextUse.Format("15:04:05")))))
				tmp := fmt.Sprintf(Render, output)

				result := []byte{}
				os.Remove("tmp.html")
				f, _ := os.Create("tmp.html")
				f.WriteString(tmp)
				chromedp.Run(ctx, fullScreenshot(chromePath, 90, &result))

				responseToUserPNGDataBase64 := base64.StdEncoding.EncodeToString(result)
				println(responseToUserPNGDataBase64)
				cq := fmt.Sprintf("[CQ:image,file=base64://%s]", responseToUserPNGDataBase64)

				// 发送最终的消息
				resp, err = http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(cq, v.GroupID, v.TargetQQ)))
				if err != nil {
					log.Printf("failed to post to api: %v\n", err)
					continue
				}
			}
			// 清空以及处理的消息
			gotTasks = nil
		} else {
			for {
				TaskQueueMutex.Lock()
				if len(TaskQueue) == 0 {
					TaskCond.Wait()
					TaskQueueMutex.Unlock()
					continue
				} else {
					for _, v := range TaskQueue {
						gotTasks = append(gotTasks, v)
					}
					TaskQueue = nil
					TaskQueueMutex.Unlock()
					break
				}
			}
		}
	}
}

func GetSendToUserBytes(msg string, groupID int64, senderUserID int64) []byte {
	sendMsgStruct := SendMsgStruct{
		MessageType: "group",
		GroupID:     groupID,
		Message:     fmt.Sprintf("[CQ:at,qq=%v]", senderUserID) + msg,
		AutoEscape:  false,
	}
	b, _ := json.Marshal(&sendMsgStruct)
	return b
}

func main() {
	r := gin.Default()
	go Sender()
	r.POST("/", func(c *gin.Context) {
		// 判断包的类型, 过滤掉除了消息包的其他包
		buf, _ := io.ReadAll(c.Request.Body)
		var header EventHeaderStruct
		json.Unmarshal(buf, &header)
		if header.PostType != "message" {
			c.Status(200)
			return
		}

		// 把message包解析出来
		var message EventMessageStrcut
		json.Unmarshal(buf, &message)

		// 过滤掉非群聊信息, 过滤掉无关群聊信息
		if message.MessageType != "group" || (message.GroupID != int64(GroupID)) {
			c.Status(200)
			return
		}

		// 过滤掉非at自己的信息
		if !strings.HasPrefix(message.RawMessage, "[CQ:at,qq=3402002560]") {
			return
		}

		userSentToBotMsg := message.RawMessage[21:]

		// 分发请求
		println("分发一个")

		if WaitTable[message.Sender.UserID] != nil && time.Now().Before(*WaitTable[message.Sender.UserID]) {
			responseToUser := "你的请求太频繁了, 你的次请求时间应该大于" + WaitTable[message.Sender.UserID].Local().Format("15:04:05")
			resp, err := http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, message.GroupID, message.Sender.UserID)))
			if err != nil {
				log.Printf("failed to post to api: %v\n", err)
				return
			}
			resp.Body.Close()
			return
		}

		responseToUser := "好的, 正在处理你的请求, 请稍后..."
		resp, err := http.Post("http://127.0.0.1:5700/send_msg", "application/json", bytes.NewBuffer(GetSendToUserBytes(responseToUser, message.GroupID, message.Sender.UserID)))
		if err != nil {
			log.Printf("failed to post to api: %v\n", err)
			return
		}
		resp.Body.Close()

		n := time.Now().Add(time.Second * time.Duration(ColdDownTime))
		WaitTable[message.Sender.UserID] = &n

		TaskQueueMutex.Lock()
		TaskQueue = append(TaskQueue, Task{
			TargetQQ: message.Sender.UserID,
			Question: userSentToBotMsg,
			GroupID:  message.GroupID,
			Nickname: message.Sender.NickName,
			NextUse:  n,
		})
		TaskQueueMutex.Unlock()
		TaskCond.Signal()

		c.Status(200)
		return
	})

	r.Run("127.0.0.1:8080")
}
