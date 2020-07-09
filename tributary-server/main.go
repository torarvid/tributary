package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	uuid "github.com/satori/go.uuid"
)

type CommandHandlerFunc func(conn *websocket.Conn, id string, message map[string]interface{})
type TreeNode struct {
	conn     *websocket.Conn
	id       string
	name     string
	children []*TreeNode
	parent   *TreeNode
}

// FIXME: this is pretty terrible. We should use custom JSON marshaling, but I couln't get it to work
func (t *TreeNode) json() map[string]interface{} {
	result := map[string]interface{}{}
	result["id"] = t.id
	result["name"] = t.name

	children := []interface{}{}
	for _, child := range t.children {
		children = append(children, child.json())
	}
	result["children"] = children
	return result
}

var (
	port         = flag.Int("port", 8081, "Port the server listens on")
	maxListeners = flag.Int("max-listeners", 3, "Max number of listeners (WebRTC peers) for a single client")
	iceServers   = flag.String("ice-servers", `[]`, "The ICE servers the peers should use")
	upgrader     = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	commandHandlers = map[string]CommandHandlerFunc{
		"START_BROADCAST":             commandStartBroadcast,
		"END_BROADCAST":               commandEndBroadcast,
		"JOIN_BROADCAST":              commandJoinBroadcast,
		"LEAVE_BROADCAST":             commandLeaveBroadcast,
		"RELAY_BROADCAST_RECEIVED":    commandRelayBroadCastReceived,
		"ICE_CANDIDATES":              commandIceCandidates,
		"ICE_CANDIDATES_RECEIVED":     commandIceCandidatesReceived,
		"SUBSCRIBE_TO_TREE_STATE":     commandSubscribeToTreeState,
		"UNSUBSCRIBE_FROM_TREE_STATE": commandUnsubscribeFromTreeState,
		"FETCH_CONFIG":                commandFetchConfig,
	}
	broadcasts         = map[string]*TreeNode{}
	peers              = map[string]*TreeNode{}
	treeStateListeners = map[string]*map[string]*websocket.Conn{}
	globalLock         = sync.Mutex{} // FIXME: yeah, I know it's horrible, but we'll fix it later
)

func main() {
	flag.Parse()
	fs := http.FileServer(http.Dir("../example"))
	http.Handle("/", fs)
	http.HandleFunc("/api/ws", handleWebSocket)
	log.Println("Server starting on port", *port)
	log.Fatal("ListenAndServe:", http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	log.Println("Incoming", r.Method, "message")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Failed to upgrade:", err)
		return
	}

	id := uuid.NewV4().String()

	for {
		var rawMessage interface{}
		if err := conn.ReadJSON(&rawMessage); err != nil {
			log.Printf("Read error: %v\n", err)
			handleDisconnect(id)
			conn.Close()
			return
		}

		messageObject, ok := rawMessage.(map[string]interface{})
		if !ok {
			sendErrorMessage(conn, "Message is not a JSON object")
			continue
		}

		command, ok := messageObject["command"].(string)
		if !ok {
			sendErrorMessage(conn, "Message is lacking a command property")
			continue
		}

		log.Printf("Received command: %v\n", command)
		if commandHandler, ok := commandHandlers[command]; ok {
			commandHandler(conn, id, messageObject)
		} else {
			sendErrorMessage(conn, fmt.Sprintf("Unknown command: %v", command))
			continue
		}
	}
}

func commandStartBroadcast(conn *websocket.Conn, id string, message map[string]interface{}) {
	name, ok := stringProp(message, "name")
	if !ok {
		sendErrorMessage(conn, "No \"name\" property specified or not a string in START_BROADCAST message")
		return
	}

	_, ok = broadcasts[name]
	if ok {
		sendErrorMessage(conn, fmt.Sprintf("Broadcast \"%v\" already exists", name))
		return
	}

	peerName, ok := stringProp(message, "peerName")
	if !ok {
		peerName = "Anonymous"
	}

	log.Printf("Peer %v starting broadcast: %v", id, name)

	globalLock.Lock()
	defer globalLock.Unlock()

	broadcasts[name] = &TreeNode{
		conn: conn,
		id:   id,
		name: peerName,
	}
	peers[id] = broadcasts[name]
	conn.WriteJSON(struct {
		Command string `json:"command"`
	}{
		"START_BROADCAST_RECEIVED",
	})

	notifyTreeListeners(name)
}

func commandEndBroadcast(conn *websocket.Conn, id string, message map[string]interface{}) {
	name, ok := stringProp(message, "name")
	if !ok {
		sendErrorMessage(conn, "No \"name\" property specified or not a string in END_BROADCAST message")
		return
	}

	globalLock.Lock()
	defer globalLock.Unlock()

	broadcast, ok := broadcasts[name]
	if !ok {
		sendErrorMessage(conn, fmt.Sprintf("Broadcast \"%v\" does not exist", name))
		return
	}

	if broadcast.id != id {
		sendErrorMessage(conn, fmt.Sprintf("Peer \"%v\" did not start broadcast \"%v\"", id, name))
		return
	}

	endBroadcast(name, broadcast)

	conn.WriteJSON(struct {
		Command string `json:"command"`
		Name    string `json:"name"`
	}{
		"END_BROADCAST_RECEIVED",
		name,
	})
}

func commandJoinBroadcast(conn *websocket.Conn, id string, message map[string]interface{}) {
	name, ok := stringProp(message, "name")
	if !ok {
		sendErrorMessage(conn, "No \"name\" property specified or not a string in JOIN_BROADCAST message")
	}

	offer, ok := objectProp(message, "offer")
	if !ok {
		sendErrorMessage(conn, "No \"offer\" property specified or not an object in JOIN_BROADCAST message")
	}

	peerName, ok := stringProp(message, "peerName")
	if !ok {
		peerName = "Anonymous"
	}

	globalLock.Lock()
	defer globalLock.Unlock()

	if broadcast, ok := broadcasts[name]; ok {
		parent := findNodeWithSpareCapacity(broadcast)
		if parent == nil {
			log.Panic("Received a nil node when inserting: %+v", broadcast)
		}

		node, ok := peers[id]
		if ok {
			log.Printf(`Peer "%v" already exists, reattaching to new parent "%v"`, id, parent.id)
			node.parent = parent
		} else {
			node = &TreeNode{
				conn:   conn,
				id:     id,
				name:   peerName,
				parent: parent,
			}
			peers[id] = node
		}

		parent.children = append(node.parent.children, node)

		log.Printf("Peer %v joining broadcast %v as a child of %v which now has %d child(ren)\n",
			id, name, parent.id, len(parent.children))

		parent.conn.WriteJSON(struct {
			Command string                 `json:"command"`
			Peer    string                 `json:"peer"`
			Offer   map[string]interface{} `json:"offer"`
		}{
			"RELAY_BROADCAST",
			id,
			offer,
		})

		notifyTreeListeners(name)
		return
	}

	sendErrorMessage(conn, fmt.Sprintf("Unknown broadcast: %v", name))
}

func commandLeaveBroadcast(conn *websocket.Conn, id string, message map[string]interface{}) {
	peerNode, ok := peers[id]
	if !ok {
		sendErrorMessage(conn, fmt.Sprintf(`Unable to find peer "%v" in LEAVE_BROADCAST message`, id))
		return
	}

	globalLock.Lock()
	defer globalLock.Unlock()

	leaveBroadcast(peerNode)

	conn.WriteJSON(struct {
		Command string `json:"command"`
	}{
		"LEAVE_BROADCAST_RECEIVED",
	})
}

func commandRelayBroadCastReceived(conn *websocket.Conn, id string, message map[string]interface{}) {
	peer, ok := stringProp(message, "peer")
	if !ok {
		sendErrorMessage(conn, "No \"peer\" property specified or not a string in RELAY_BROADCAST_RECEIVED message")
	}

	answer, ok := objectProp(message, "answer")
	if !ok {
		sendErrorMessage(conn, "No \"answer\" property specified or not an object in RELAY_BROADCAST_RECEIVED message")
	}

	log.Printf("Peer %v responding to %v with answer: %+v\n", id, peer, answer)

	if peerNode, ok := peers[peer]; ok {
		peerNode.conn.WriteJSON(struct {
			Command string                 `json:"command"`
			Peer    string                 `json:"peer"`
			Answer  map[string]interface{} `json:"answer"`
		}{
			"JOIN_BROADCAST_RECEIVED",
			id,
			answer,
		})
		return
	}

	sendErrorMessage(conn, fmt.Sprintf("Unknown peer: %v", peer))
}

func commandIceCandidates(conn *websocket.Conn, id string, message map[string]interface{}) {
	peer, ok := stringProp(message, "peer")
	if !ok {
		sendErrorMessage(conn, "No \"peer\" property specified or not a string in ICE_CANDIDATES message")
	}

	candidates, ok := arrayProp(message, "candidates")
	if !ok {
		sendErrorMessage(conn, "No \"candidates\" property specified or not an array in ICE_CANDIDATES message")
	}

	log.Printf("Peer %v sending ICE candidates to peer %v: %+v", id, peer, candidates)

	if peerNode, ok := peers[peer]; ok {
		peerNode.conn.WriteJSON(struct {
			Command    string        `json:"command"`
			Peer       string        `json:"peer"`
			Candidates []interface{} `json:"candidates"`
		}{
			"ICE_CANDIDATES",
			id,
			candidates,
		})
		return
	}

	sendErrorMessage(conn, fmt.Sprintf("Unknown peer: %v", peer))
}

func commandIceCandidatesReceived(conn *websocket.Conn, id string, message map[string]interface{}) {
	if peer, ok := stringProp(message, "peer"); ok {
		if peerNode, ok := peers[peer]; ok {

			log.Printf("Peer %v ack-ing ICE candidates from peer %v", id, peer)

			peerNode.conn.WriteJSON(struct {
				Command string `json:"command"`
				Peer    string `json:"peer"`
			}{
				"ICE_CANDIDATES_RECEIVED",
				id,
			})
			return
		}

		sendErrorMessage(conn, fmt.Sprintf("Unknown peer: %v", peer))
	} else {
		sendErrorMessage(conn, "No \"peer\" property specified or not a string in ICE_CANDIDATES_RECEIVED message")
	}
}

func commandSubscribeToTreeState(conn *websocket.Conn, id string, message map[string]interface{}) {
	name, ok := stringProp(message, "name")
	if !ok {
		sendErrorMessage(conn, "No \"name\" property specified or not a string in SUBSCRIBE_TO_TREE_STATE message")
		return
	}

	globalLock.Lock()
	defer globalLock.Unlock()

	_, ok = broadcasts[name]
	if !ok {
		sendErrorMessage(conn, fmt.Sprintf("Unknown broadcast: %v", name))
		return
	}

	listeners, ok := treeStateListeners[name]
	if !ok {
		listeners = &map[string]*websocket.Conn{}
		treeStateListeners[name] = listeners
	}

	(*listeners)[id] = conn
	conn.WriteJSON(struct {
		Command string `json:"command"`
	}{
		"SUBSCRIBE_TO_TREE_STATE_RECEIVED",
	})

	notifyTreeListeners(name)
}

func commandUnsubscribeFromTreeState(conn *websocket.Conn, id string, message map[string]interface{}) {
	name, ok := stringProp(message, "name")
	if !ok {
		sendErrorMessage(conn, "No \"name\" property specified or not a string in UNSUBSCRIBE_FROM_TREE_STATE message")
		return
	}

	globalLock.Lock()
	defer globalLock.Unlock()

	_, ok = broadcasts[name]
	if !ok {
		sendErrorMessage(conn, fmt.Sprintf("Unknown broadcast: %v", name))
		return
	}

	listeners, ok := treeStateListeners[name]
	if !ok {
		return
	}

	delete(*listeners, id)

	conn.WriteJSON(struct {
		Command string `json:"command"`
	}{
		"UNSUBSCRIBE_FROM_TREE_STATE_RECEIVED",
	})
}

func commandFetchConfig(conn *websocket.Conn, id string, message map[string]interface{}) {
	var iceJSON []interface{}
	decoder := json.NewDecoder(strings.NewReader(*iceServers))
	if err := decoder.Decode(&iceJSON); err != nil {
		log.Println(err)
		sendErrorMessage(conn, "Invalid ICE server JSON")
		return
	}

	conn.WriteJSON(struct {
		Command    string        `json:"command"`
		IceServers []interface{} `json:"iceServers"`
	}{
		"FETCH_CONFIG_RECEIVED",
		iceJSON,
	})
}

func findNodeWithSpareCapacity(root *TreeNode) *TreeNode {
	queue := []*TreeNode{root}
	var node *TreeNode

	for len(queue) > 0 {
		node, queue = queue[0], queue[1:]

		// FIXME: we can be more clever here in order to spread the load between the children better
		if len(node.children) < *maxListeners {
			return node
		}

		queue = append(queue, node.children...)
	}

	return nil
}

func endBroadcast(broadcastName string, broadcast *TreeNode) {

	command := struct {
		Command string `json:"command"`
		Name    string `json:"name"`
	}{
		"BROADCAST_ENDED",
		broadcastName,
	}

	var destroyTree func(node *TreeNode)
	destroyTree = func(node *TreeNode) {
		node.conn.WriteJSON(command)
		delete(peers, node.id)

		for _, child := range node.children {
			destroyTree(child)
		}
	}

	destroyTree(broadcast)
	delete(broadcasts, broadcastName)
}

func leaveBroadcast(node *TreeNode) {
	if node == nil {
		return
	}

	broadcast := node
	for broadcast.parent != nil {
		broadcast = broadcast.parent
	}

	broadcastName := ""
	for name, broadcastNode := range broadcasts {
		if broadcastNode.id == broadcast.id {
			broadcastName = name
			break
		}
	}

	if broadcastName == "" {
		log.Printf(`Unable to find the name of broadcast "%v"`, broadcast.id)
		return
	}

	for i, child := range node.parent.children {
		if child.id == node.id {
			node.parent.children = append(node.parent.children[:i], node.parent.children[i+1:]...)
			break
		}
	}

	for _, child := range node.children {
		child.parent = nil
		child.conn.WriteJSON(struct {
			Command string `json:"command"`
			Name    string `json:"name"`
		}{
			"RECONNECT_TO_BROADCAST",
			broadcastName,
		})
	}

	node.children = []*TreeNode{}
	delete(peers, node.id)
	notifyTreeListeners(broadcastName)
}

func handleDisconnect(id string) {
	globalLock.Lock()
	defer globalLock.Unlock()

	// check first if this connection was broadcasting, if so, shut down the broadcast
	for broadcastName, broadcast := range broadcasts {
		if broadcast.id == id {
			log.Printf(`Peer "%v" disconnected, ending broadcast "%v"`, id, broadcastName)
			endBroadcast(broadcastName, broadcast)
			return
		}
	}

	leaveBroadcast(peers[id])
}

func notifyTreeListeners(broadcastName string) {
	broadcast, ok := broadcasts[broadcastName]
	if !ok {
		log.Printf("Unknown broadcast in notifyTreeListeners: %v\n", broadcastName)
		return
	}
	listeners, ok := treeStateListeners[broadcastName]
	if !ok {
		log.Printf("No tree state listeners for broadcast: %v\n", broadcastName)
		return
	}

	for _, conn := range *listeners {
		conn.WriteJSON(struct {
			Command string                 `json:"command"`
			Tree    map[string]interface{} `json:"tree"`
		}{
			"TREE_STATE_CHANGED",
			broadcast.json(),
		})
	}
}

func sendErrorMessage(conn *websocket.Conn, message string) {
	log.Println(message)
	conn.WriteJSON(struct {
		Command string `json:"command"`
		Message string `json:"message"`
	}{"ERROR", message})
}

func sendErrorMessageAndCode(conn *websocket.Conn, message string, errorCode int) {
	log.Println(message)
	conn.WriteJSON(struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}{
		message,
		errorCode,
	})
}
