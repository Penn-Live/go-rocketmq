package rocketmq

import (
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/powxiao/go-rocketmq/rocketmq/header"
)

/*
1. 每次消息会发送失败或者超时，会重试（上限15次）
2.

producer.setNamesrvAddr("127.0.0.1:9876");
message = new Message(topic, new byte[] {'a'});
producer.start()
getMQClientFactory().registerProducer
createTopicRoute()
SendResult sendResult = producer.send(message);

SendMessageRequestHeader
CommunicationMode
TopicRoute:BrokerData/queueData

SelectMessageQueueByHash
*/

type SendCallBack interface {
	OnSuccess(result *SendResult)
}

type Producer interface {
	Start() error
	ShutDown()
	Send(msg *MessageExt) (sendResult *SendResult, err error)
	SendAsync(msg *MessageExt, sendCallBack SendCallBack)
	SendOneWay(msg *MessageExt)
}

// DefaultProducer ...
type DefaultProducer struct {
	conf              *Config
	producerGroup     string
	communicationMode string
	mqClient          *MqClient
}

// NewDefaultProducer ...
func NewDefaultProducer(name string, conf *Config) Producer {
	if conf == nil {
		conf = &Config{
			NameServer:   os.Getenv("ROCKETMQ_NAMESVR"),
			InstanceName: "DEFAULT",
		}
	}

	if conf.ClientIp == "" {
		conf.ClientIp = DEFAULT_IP
	}

	pullMessageService := NewPullMessageService()
	remotingClient := NewDefaultRemotingClient()
	mqClient := NewMqClient()
	producer := &DefaultProducer{
		producerGroup: name,
		mqClient:      mqClient,
	}

	mqClient.producerTable[name] = producer
	mqClient.remotingClient = remotingClient
	mqClient.conf = conf
	mqClient.clientId = conf.ClientIp + "@" + strconv.Itoa(os.Getpid())
	mqClient.pullMessageService = pullMessageService

	return producer
}

func (d *DefaultProducer) Start() error {
	d.mqClient.start()
	return nil
}

func (d *DefaultProducer) ShutDown() {

}

func (d *DefaultProducer) SendOneWay(msg *MessageExt) {

}

func (d *DefaultProducer) SendAsync(msg *MessageExt, sendCallback SendCallBack) {

}

func (d *DefaultProducer) Send(msg *MessageExt) (sendResult *SendResult, err error) {
	err = d.checkMessage(msg)
	if err != nil {
		return
	}
	var topicPublishInfo *TopicPublishInfo
	topicPublishInfo, err = d.mqClient.tryToFindTopicPublishInfo(msg.Topic)
	if err != nil {
		return
	}

	if topicPublishInfo.JudgeTopicPublishInfoOk() == false {
		err = errors.New("topicPublishInfo is error,topic=" + msg.Topic)
		return
	}

	var (
		lastFailedBroker string
		messageQueue     MessageQueue
	)

	communicationMode := "Sync" //默认是同步发送
	timeout := time.Second * 5  //默认发送超时时间为5s

	//retry to send message
	for times := 0; times < 15; times++ {
		messageQueue, err = selectOneMessageQueue(topicPublishInfo, lastFailedBroker) //没有选择ActiveMessageQueue,默认选第一个
		if err != nil {
			return
		}
		begin := time.Now()
		sendResult, err = d.doSendMessage(msg, messageQueue, communicationMode, nil, topicPublishInfo, int64(timeout))
		end := time.Now().Sub(begin)
		Printf("send message cost %v(s)", end.Seconds())
		switch communicationMode {
		case "Async":
			return
		case "OneWay":
			return
		case "Sync":
			if sendResult.sendStatus != SendOK {
				continue
			}
			return
		default:
			break
		}
		if err != nil {
			return
		}
	}
	return
}

func (d *DefaultProducer) tryToCompressMessage(message *MessageExt) (compressedFlag int, err error) {
	if len(message.Body) < 1024*4 {
		compressedFlag = 0
		return
	}

	compressedFlag = int(CompressedFlag)
	var compressBody []byte
	compressBody, err = CompressWithLevel(message.Body, 5)
	message.Body = compressBody
	return
}

func (d *DefaultProducer) doSendMessage(msg *MessageExt, messageQueue MessageQueue, communicationMode string,
	sendCallback SendCallBack, info *TopicPublishInfo, timeout int64) (sendResult *SendResult, err error) {

	var (
		brokerAddr          string
		sysFlag             int
		compressMessageFlag int
	)
	sysFlag = sysFlag | compressMessageFlag
	compressMessageFlag, err = d.tryToCompressMessage(msg)
	if err != nil {
		return
	}
	msg.GeneratorMsgUniqueKey()
	sendMessageHeader := &header.SendMessageRequestHeader{
		ProducerGroup:         d.producerGroup,
		Topic:                 msg.Topic,
		DefaultTopic:          DEFAULT_TOPIC,
		DefaultTopicQueueNums: 4,
		QueueId:               messageQueue.queueId,
		SysFlag:               sysFlag,
		BornTimestamp:         CurrentTimeMillisInt64(),
		Flag:                  msg.Flag,
		Properties:            MessageProperties2String(msg.Properties),
		UnitMode:              false,
		//ReconsumeTimes:        msg.GetReconsumeTimes(),
		//MaxReconsumeTimes:     msg.GetMaxReconsumeTimes(),
	}
	brokerAddr = d.mqClient.fetchMasterBrokerAddress(messageQueue.brokerName)
	if brokerAddr == "" {
		err = errors.New("The broker[" + messageQueue.brokerName + "] not exist")
		return
	}

	remoteClient := d.mqClient.remotingClient
	remotingCommand := NewRemotingCommandWithBody(SEND_MESSAGE, sendMessageHeader, msg.Body)

	switch communicationMode {
	case "Async":
		err = remoteClient.invokeAsync(brokerAddr, remotingCommand, timeout, func(responseFuture *ResponseFuture) {
			if sendCallback == nil && responseFuture.responseCommand != nil {
				sendResult, err = processSendResponse(messageQueue.brokerName, msg, responseFuture.responseCommand)
			}
			if responseFuture.responseCommand != nil {
				sendResult, err = processSendResponse(messageQueue.brokerName, msg, responseFuture.responseCommand)
				if sendCallback != nil {
					sendCallback.OnSuccess(sendResult)
				}
			} else {
			}
		})
		break
	case "Sync":
		var response *RemotingCommand
		response, err = remoteClient.invokeSync(brokerAddr, remotingCommand, timeout)
		if err != nil {
			Println(err)
			return
		}
		sendResult, err = processSendResponse(messageQueue.brokerName, msg, response)
		break
	case "OneWay":
		err = remoteClient.invokeOneWay(brokerAddr, remotingCommand, timeout)
		if err != nil {
			Println(err)
			return
		}
		break
	default:
		Printf("unknown producer communicate mode")
		break
	}
	if err != nil {
		Println(err)
		return
	}
	return
}

func processSendResponse(brokerName string, message *MessageExt,
	response *RemotingCommand) (sendResult *SendResult, err error) {
	sendResult = &SendResult{}
	switch response.Code {
	case FLUSH_DISK_TIMEOUT:
		{
			sendResult.sendStatus = FlushDiskTimeout
			break
		}
	case FLUSH_SLAVE_TIMEOUT:
		{
			sendResult.sendStatus = FlushSlaveTimeout
			break
		}
	case SLAVE_NOT_AVAILABLE:
		{
			sendResult.sendStatus = SlaveNotAvaliable
			break
		}
	case SUCCESS:
		{
			sendResult.sendStatus = SendOK
			break
		}
	default:
		err = errors.New("response.Code error_code=" + strconv.Itoa(int(response.Code)))
		return
	}
	var responseHeader = &SendMessageResponseHeader{}
	if response.ExtFields != nil {
		responseHeader.FromMap(response.ExtFields) //change map[string]interface{} into CustomerHeader struct
	}
	sendResult.msgID = message.Properties[PROPERTY_UNIQ_CLIENT_MESSAGE_ID_KEYIDX]
	sendResult.offsetMsgID = responseHeader.MsgId
	sendResult.queueOffset = responseHeader.QueueOffset
	sendResult.transactionID = responseHeader.TransactionId
	messageQueue := MessageQueue{topic: message.Topic, brokerName: brokerName,
		queueId: responseHeader.QueueId}
	sendResult.messageQueue = messageQueue
	var regionId = responseHeader.MsgRegion
	if len(regionId) == 0 {
		regionId = "DefaultRegion"
	}
	sendResult.regionID = regionId
	return
}

func (d *DefaultProducer) SendWithTimeout(msg MessageExt, timeout int64) (sendResult *SendResult, err error) {
	return nil, nil
}

func (d *DefaultProducer) checkMessage(msg *MessageExt) error {
	return nil
}

//if first select : random one
//if has error broker before ,skip the err broker
func selectOneMessageQueue(topicPublishInfo *TopicPublishInfo, lastFailedBroker string) (mqQueue MessageQueue, err error) {
	queueIndex := topicPublishInfo.FetchQueueIndex()
	queues := topicPublishInfo.MessageQueueList
	if len(lastFailedBroker) == 0 {
		mqQueue = queues[queueIndex]
		return
	}
	for i := 0; i < len(queues); i++ {
		nowQueueIndex := queueIndex + i
		if nowQueueIndex >= len(queues) {
			nowQueueIndex = nowQueueIndex - len(queues)
		}
		if lastFailedBroker == queues[nowQueueIndex].brokerName {
			continue
		}
		mqQueue = queues[nowQueueIndex]
		return
	}
	err = errors.New("send to [" + lastFailedBroker + "] fail,no other broker")
	return
}