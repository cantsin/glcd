package main

import (
  "bytes"
  "crypto/sha512"
  "encoding/json"
  "errors"
  "fmt"
  // "github.com/gamelost/bot3server/server"
  nsq "github.com/gamelost/go-nsq"
  suture "github.com/thejerf/suture"
  // irc "github.com/gamelost/goirc/client"
  "labix.org/v2/mgo"
  "labix.org/v2/mgo/bson"
  "os"
  "os/signal"
  "syscall"
  "time"
)

func main() {
  // the quit channel
  sigChan := make(chan os.Signal, 1)
  signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

  glcd := &GLCD{QuitChan: sigChan}

  config := ReadConfiguration()
  config.PrintConfiguration()

  glcd.init(config)

  // receiving quit shuts down
  <-glcd.QuitChan
}

type GLCD struct {
  Supervisor *suture.Supervisor

  Online bool
  Config *GLCConfig

  // NSQ input/output
  NSQWriter             *nsq.Writer
  GLCDaemonTopic        *nsq.Reader
  GLCDaemonTopicChannel string
  Clients               map[string]*GLCClient

  // game state channels
  HeartbeatChan   chan *Heartbeat
  BroadcastChan   chan *Message
  AuthChan        chan *PlayerAuthInfo
  PlayerStateChan chan *Message

  QuitChan chan os.Signal

  MongoSession *mgo.Session
  MongoDB      *mgo.Database
}

func (glcd *GLCD) init(config *GLCConfig) error {
  glcd.Supervisor = suture.NewSimple("GlcdSupervisor")

  glcd.Config = config
  glcd.Online = false

  glcd.Clients = map[string]*GLCClient{}

  // Connect to Mongo.
  glcd.setupMongoDBConnection()

  // set up channels
  glcd.setupTopicChannels()

  // Create the channel, by connecting to lookupd. (TODO; if it doesn't
  // exist. Also do it the right way with a Register command?)
  glcd.NSQWriter = nsq.NewWriter(glcd.Config.NSQ.Address)
  glcd.NSQWriter.Publish(glcd.Config.NSQ.PublishTopic, []byte("{\"client\":\"server\"}"))

  // set up reader for glcdTopic
  reader, err := nsq.NewReader(glcd.Config.NSQ.ReadTopic, "main")
  if err != nil {
    glcd.QuitChan <- syscall.SIGINT
  }
  glcd.GLCDaemonTopic = reader
  glcd.GLCDaemonTopic.AddHandler(glcd)
  glcd.GLCDaemonTopic.ConnectToLookupd(glcd.Config.NSQ.LookupdAddress)

  // Supervisor Tree services to handle concurrent events
  glcd.Supervisor.Add(&HeartbeatService{glcd: glcd})
  glcd.Supervisor.Add(&PlayerAuthService{glcd: glcd})
  glcd.Supervisor.Add(&BroadcastService{glcd: glcd})
  glcd.Supervisor.Add(&PlayerStateService{glcd: glcd})
  glcd.Supervisor.Add(&ClientCleanupService{glcd: glcd})

  go glcd.Supervisor.ServeBackground()

  return nil
}

func (glcd *GLCD) setupTopicChannels() {
  // set up channels
  glcd.HeartbeatChan = make(chan *Heartbeat)
  glcd.BroadcastChan = make(chan *Message)
  glcd.AuthChan = make(chan *PlayerAuthInfo)
  glcd.PlayerStateChan = make(chan *Message)
}

func (glcd *GLCD) setupMongoDBConnection() {

  // Connect to Mongo.
  var err error

  glcd.MongoSession, err = mgo.Dial(glcd.Config.Mongo.Servers)
  if err != nil {
    panic(fmt.Sprintf("Could not connect to database %s."))
  }

  glcd.MongoDB = glcd.MongoSession.DB(glcd.Config.Mongo.DB)
}

func (glcd *GLCD) Publish(msg *Message) {
  encodedRequest, _ := json.Marshal(*msg)
  glcd.NSQWriter.Publish(glcd.Config.NSQ.PublishTopic, encodedRequest)
}

// Send a zone file update.
func (glcd *GLCD) SendZones() {
  fmt.Println("SendZones --")
  c := glcd.MongoDB.C("zones")
  q := c.Find(nil)

  if q == nil {
    glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("No zones found")})
  } else {
    fmt.Println("Publishing zones to clients")
    var results []interface{}
    err := q.All(&results)
    if err == nil {
      for _, res := range results {
        fmt.Printf("Res: is %+v", res)
        glcd.Publish(&Message{Type: "updateZone", Data: res.(bson.M)}) // dump res as a JSON string
      }
    } else {
      glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("Unable to fetch zones: %v", err)})
    }
  }
}

func (glcd *GLCD) SendZone(zone *Zone) {
  c := glcd.MongoDB.C("zones")
  query := bson.M{"zone": zone.Name}
  results := c.Find(query)

  if results == nil {
    glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("No such zone '%s'", zone.Name)})
  } else {
    var res interface{}
    err := results.One(&res)
    if err == nil {
      glcd.Publish(&Message{Type: "zone", Data: res.(string)})
    } else {
      glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("Unable to fetch zone: %v", err)})
    }
  }
}

// Send a zone file update.
func (glcd *GLCD) UpdateZone(zone *Zone) {
  query := bson.M{"zone": zone.Name}
  zdata := ZoneInfo{}
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
    glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("Unable to update zone: %v", err)})
  } else {
    glcd.Publish(&Message{Type: "error", Data: fmt.Sprintf("Updated zone '%s'", zone.Name)})
  }
}

func (glcd *GLCD) HandleMessage(nsqMessage *nsq.Message) error {

  // fmt.Println("-------")
  // fmt.Printf("Received message %s\n\n", nsqMessage.Body)
  // fmt.Println("-------")
  msg := &Message{}

  err := json.Unmarshal(nsqMessage.Body, &msg)

  if err != nil {
    fmt.Printf(err.Error())
    return err
  }

  var dataMap map[string]interface{}
  var ok bool

  if msg.Data != nil {
    dataMap, ok = msg.Data.(map[string]interface{})
  } else {
    dataMap = make(map[string]interface{})
    ok = true
  }

  if !ok {
    return nil
  }

  // UNIMPLEMENTED TYPES: sendZones, error add new/future handler
  // functions in glcd-handlers.go
  if msg.Type == "playerState" {
    glcd.HandlePlayerState(msg, dataMap)
  } else if msg.Type == "connected" {
    glcd.HandleConnected(msg, dataMap)
  } else if msg.Type == "chat" {
    glcd.HandleChat(msg, msg.Data)
  } else if msg.Type == "heartbeat" {
    glcd.HandleHeartbeat(msg, dataMap)
  } else if msg.Type == "broadcast" {
    glcd.HandleBroadcast(msg, dataMap)
  } else if msg.Type == "playerAuth" {
    glcd.HandlePlayerAuth(msg, dataMap)
  } else {
    fmt.Printf("Unable to determine handler for message: %+v\n", msg)
  }

  return nil
}

func (glcd *GLCD) isPasswordCorrect(name string, password string) (bool, error) {
  c := glcd.MongoDB.C("users")
  authInfo := PlayerAuthInfo{}
  query := bson.M{"user": name}
  err := c.Find(query).One(&authInfo)

  if err != nil {
    return false, err
  }

  return password == authInfo.Password, nil
}

func generateSaltedPasswordHash(password string, salt []byte) ([]byte, error) {
  hash := sha512.New()
  //hash.Write(server_salt)
  hash.Write(salt)
  hash.Write([]byte(password))
  return hash.Sum(salt), nil
}

func (glcd *GLCD) getUserPasswordHash(name string) ([]byte, error) {
  return nil, nil
}

func (glcd *GLCD) isPasswordCorrectWithHash(name string, password string, salt []byte) (bool, error) {
  expectedHash, err := glcd.getUserPasswordHash(name)

  if err != nil {
    return false, err
  }

  if len(expectedHash) != 32+sha512.Size {
    return false, errors.New("Wrong size")
  }

  actualHash := sha512.New()
  actualHash.Write(salt)
  actualHash.Write([]byte(password))

  return bytes.Equal(actualHash.Sum(nil), expectedHash[32:]), nil
}
