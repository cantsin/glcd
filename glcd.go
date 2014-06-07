package main

import (
	iniconf "code.google.com/p/goconf/conf"
	"encoding/json"
	"fmt"
	// "github.com/gamelost/bot3server/server"
	nsq "github.com/gamelost/go-nsq"
	// irc "github.com/gamelost/goirc/client"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	GLCD_CONFIG = "glcd.config"
)

type Message map[string]interface{}

func main() {
	// the quit channel
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	// read in necessary configuration
	configFile, err := iniconf.ReadConfigFile(GLCD_CONFIG)
	if err != nil {
		log.Fatal("Unable to read configuration file. Exiting now.")
	}

	glcd := &GLCD{}
	glcd.QuitChan = sigChan
	glcd.init(configFile)

	// receiving quit shuts down
	<-sigChan
}

type GLCClient struct {
	Name      string
	Topic     string
	Writer    *nsq.Writer
	Heartbeat time.Time
	Clientid  string
	State     *Message // json representation of player state.
}

// struct type for Bot3
type GLCD struct {
	Online     bool
	ConfigFile *iniconf.ConfigFile

	// NSQ input/output to bot3Server
	GLCInput *nsq.Reader
	Clients  map[string]*GLCClient

	QuitChan chan os.Signal

	MongoSession *mgo.Session
	MongoDB      *mgo.Database
}

func (glcd *GLCD) init(conf *iniconf.ConfigFile) error {

	glcd.ConfigFile = conf
	glcd.Online = false

	glcd.Clients = map[string]*GLCClient{}

	// Connect to Mongo.
	servers, err := glcd.ConfigFile.GetString("mongo", "servers")

	if err != nil {
		return fmt.Errorf("Mongo: No server configured.")
	}

	glcd.MongoSession, err = mgo.Dial(servers)

	if err != nil {
	}

	db, err := glcd.ConfigFile.GetString("mongo", "db")

	if err != nil {
		return fmt.Errorf("Mongo: No database configured.")
	}

	glcd.MongoDB = glcd.MongoSession.DB(db)

	lookupdAddress, _ := conf.GetString("nsq", "lookupd-address")
	nsqdAddress, _ := conf.GetString("nsq", "nsqd-address")
	serverTopic, _ := conf.GetString("nsq", "server-topic")
	serverChannel, _ := conf.GetString("nsq", "server-channel")

	// Create the channel, by connecting to lookupd. (TODO; if it doesn't
	// exist. Also do it the right way with a Register command?)
	writer := nsq.NewWriter(nsqdAddress)
	writer.Publish(serverTopic, []byte("{\"client\":\"server\"}"))

	// set up listener for heartbeat from bot3server
	reader, err := nsq.NewReader(serverTopic, serverChannel)
	if err != nil {
		panic(err)
		glcd.QuitChan <- syscall.SIGINT
	}
	glcd.GLCInput = reader

	glcd.GLCInput.AddHandler(glcd)
	glcd.GLCInput.ConnectToLookupd(lookupdAddress)

	// Spawn goroutine to clear out clients who don't send hearbeats
	// anymore.
	go glcd.CleanupClients()

	return nil
}

func (cl *GLCClient) Publish(msg *Message) {
	if cl.Writer == nil {
		args := strings.SplitN(cl.Clientid, ":", 3)
		if len(args) != 3 {
			return
		}
		host, port, topic := args[0], args[1], args[2]
		// log.Printf("cl.publish: Attempting to connect to '" + host + ":" + port + "'")
		cl.Writer = nsq.NewWriter(host + ":" + port)
		cl.Topic = topic
	}
	encodedRequest, _ := json.Marshal(*msg)
	cl.Writer.Publish(cl.Topic, encodedRequest)
}

func (cl *GLCClient) SendCommand(command string, data *Message) {
	msg := Message{}
	msg["command"] = command
	msg["data"] = data
	cl.Publish(&msg)
}

func (cl *GLCClient) SendMessage(text string) {
	cl.SendCommand("message", &Message{"data": text})
}

func (glcd *GLCD) SendCommandAll(command string, data *Message) {
	msg := Message{}
	msg["command"] = command
	msg["data"] = data
	for _, v := range glcd.Clients {
		v.Publish(&msg)
	}
}

func (glcd *GLCD) CleanupClients() error {
	for {
		exp := time.Now().Unix()
		<-time.After(time.Second * 10)
		// Expire any clients who haven't sent a heartbeat in the last 10 seconds.
		for k, v := range glcd.Clients {
			if v.Heartbeat.Unix() < exp {
				delete(glcd.Clients, k)
				glcd.SendCommandAll("playerGone", &Message{"client": k})
			}
		}
	}
}

// Send a zone file update.
func (glcd *GLCD) SendZones(cl *GLCClient) {
	c := glcd.MongoDB.C("zones")
	q := c.Find(nil)

	if q == nil {
		cl.SendMessage(fmt.Sprintf("No zones found"))
	} else {
		var results []interface{}
		err := q.All(&results)
		if err == nil {
			for _, res := range results {
				cl.SendCommand("updateZone", &Message{"data": res})
			}
		} else {
			cl.SendMessage(fmt.Sprintf("Unable to fetch zones: %v", err))
		}
	}
}

func (glcd *GLCD) SendZone(cl *GLCClient, zone string) {
	c := glcd.MongoDB.C("zones")
	query := bson.M{"zone": zone}
	results := c.Find(query)

	if results == nil {
		cl.SendMessage(fmt.Sprintf("No such zone '%s'", zone))
	} else {
		var res interface{}
		err := results.One(&res)
		if err == nil {
			cl.SendCommand("updateZone", &Message{"data": res})
		} else {
			cl.SendMessage(fmt.Sprintf("Unable to fetch zone: %v", err))
		}
	}
}

// Send a zone file update.
func (glcd *GLCD) UpdateZone(cl *GLCClient, zone string, zdata interface{}) {
	query := bson.M{"zone": zone}

	c := glcd.MongoDB.C("zones")
	val := bson.M{"type": "zone", "zdata": zdata, "timestamp": time.Now()}
	change := bson.M{"$set": val}

	err := c.Update(query, change)

	if err == mgo.ErrNotFound {
		val["id"], _ = c.Count()
		change = bson.M{"$set": val}
		err = c.Update(query, change)
	}

	if err != nil {
		cl.SendMessage(fmt.Sprintf("Unable to update zone: %v", err))
	} else {
		cl.SendMessage(fmt.Sprintf("Updated zone '%s'", zone))
	}
}

func (glcd *GLCD) HandleMessage(message *nsq.Message) error {
	msg := Message{}

	err := json.Unmarshal(message.Body, &msg)

	if err != nil {
		return fmt.Errorf("Not a JSON interface")
	}

	// If "client" is not in the JSON received, dump it.
	cdata, ok := msg["client"]
	if !ok {
		return fmt.Errorf("No client provided")
	}

	// If "client" from JSON is
	clientid, ok := cdata.(string)
	if !ok {
		return fmt.Errorf("Invalid format: client is not a string.")
	}

	// Ignore our silly "create a topic" message.
	if clientid == "server" {
		return nil
	}

	// Make sure client exists in glcd.Clients
	cl, exists := glcd.Clients[clientid]
	if !exists {
		//cl = &GLCClient{}
		//cl.Clientid = clientid
		//glcd.Clients[clientid] = cl
	}
	cl.Heartbeat = time.Now()

	// Now perform the client's action.
	cmddata, ok := msg["command"]
	if !ok {
		// Lacking a command is okay - It's a heartbeat.
		return nil
	}

	// If "client" from JSON is
	command, ok := cmddata.(string)
	if !ok {
		return fmt.Errorf("Invalid format: command is not a string.")
	}
	// log.Printf("We got a command: ", command)

	switch command {
	case "ping":
		cl.Publish(&Message{"pong": fmt.Sprint(time.Now())})
		break
	case "playerState":
		_, ok := msg["data"]
		if ok {
			cl.State = &msg
			glcd.SendCommandAll("playerState", &msg)
		}
		break
	case "updateZone":
		args, ok := msg["data"]
		if !ok {
			break
		}
		margs, ok := args.(Message)
		if !ok {
			break
		}
		zonei, ok := margs["zone"]
		if !ok {
			break
		}
		zone, ok := zonei.(string)
		if !ok {
			break
		}

		zdata, ok := margs["data"]
		if !ok {
			break
		}

		glcd.UpdateZone(cl, zone, zdata)
	case "sendZone":
		args, ok := msg["data"]
		if !ok {
			log.Printf("No data in sendZone")
			break
		}
		log.Println("data:")
		log.Println(args)
		margs, ok := args.(map[string]interface{})
		if !ok {
			log.Printf("args is not a message in sendZone")
			break
		}
		zonei, ok := margs["zone"]
		if !ok {
			log.Printf("No zone in sendZone")
			break
		}
		zone, ok := zonei.(string)
		if !ok {
			log.Printf("Zone is not a string in sendZone")
			break
		}

		glcd.SendZone(cl, zone)
		break
	case "connected":
		// Send all Zones
		glcd.SendZones(cl)
		// Send all player states.
		for _, v := range glcd.Clients {
			cl.SendCommand("playerState", v.State)
		}
		break
	case "wall":
		for _, v := range glcd.Clients {
			v.Publish(&msg)
		}
	}
	return nil
}