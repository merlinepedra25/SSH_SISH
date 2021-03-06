package utils

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
	"github.com/vulcand/oxy/roundrobin"
)

// upgrader is the default WS upgrader that we use for webconsole clients.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// WebClient represents a primitive web console client. It maintains
// references that allow us to communicate and track a client connection.
type WebClient struct {
	Conn    *websocket.Conn
	Console *WebConsole
	Send    chan []byte
	Route   string
}

// WebConsole represents the data structure that stores web console client information.
// Clients is a map[string][]*WebClient.
// RouteTokens is a map[string]string.
type WebConsole struct {
	Clients     *sync.Map
	RouteTokens *sync.Map
	State       *State
}

// NewWebConsole sets up the WebConsole.
func NewWebConsole() *WebConsole {
	return &WebConsole{
		Clients:     &sync.Map{},
		RouteTokens: &sync.Map{},
	}
}

// HandleRequest handles an incoming web request, handles auth, and then routes it.
func (c *WebConsole) HandleRequest(proxyUrl string, hostIsRoot bool, g *gin.Context) {
	userAuthed := false
	userIsAdmin := false
	if (viper.GetBool("admin-console") && viper.GetString("admin-console-token") != "") && (g.Request.URL.Query().Get("x-authorization") == viper.GetString("admin-console-token") || g.Request.Header.Get("x-authorization") == viper.GetString("admin-console-token")) {
		userIsAdmin = true
		userAuthed = true
	}

	tokenInterface, ok := c.RouteTokens.Load(proxyUrl)
	if ok {
		routeToken, ok := tokenInterface.(string)
		if viper.GetBool("service-console") && ok && (g.Request.URL.Query().Get("x-authorization") == routeToken || g.Request.Header.Get("x-authorization") == routeToken) {
			userAuthed = true
		}
	}

	if strings.HasPrefix(g.Request.URL.Path, "/_sish/console/ws") && userAuthed {
		c.HandleWebSocket(proxyUrl, g)
		return
	} else if strings.HasPrefix(g.Request.URL.Path, "/_sish/console") && userAuthed {
		c.HandleTemplate(proxyUrl, hostIsRoot, userIsAdmin, g)
		return
	} else if strings.HasPrefix(g.Request.URL.Path, "/_sish/api/disconnectclient/") && userIsAdmin {
		c.HandleDisconnectClient(proxyUrl, g)
		return
	} else if strings.HasPrefix(g.Request.URL.Path, "/_sish/api/disconnectroute/") && userIsAdmin {
		c.HandleDisconnectRoute(proxyUrl, g)
		return
	} else if strings.HasPrefix(g.Request.URL.Path, "/_sish/api/clients") && hostIsRoot && userIsAdmin {
		c.HandleClients(proxyUrl, g)
		return
	}
}

// HandleTemplate handles rendering the console templates.
func (c *WebConsole) HandleTemplate(proxyUrl string, hostIsRoot bool, userIsAdmin bool, g *gin.Context) {
	if hostIsRoot && userIsAdmin {
		g.HTML(http.StatusOK, "routes", nil)
		return
	}

	if c.RouteExists(proxyUrl) {
		g.HTML(http.StatusOK, "console", nil)
		return
	}

	err := g.AbortWithError(http.StatusNotFound, fmt.Errorf("cannot find connection for host: %s", proxyUrl))
	if err != nil {
		log.Println("Aborting with error", err)
	}
}

// HandleWebSocket handles the websocket route.
func (c *WebConsole) HandleWebSocket(proxyUrl string, g *gin.Context) {
	conn, err := upgrader.Upgrade(g.Writer, g.Request, nil)
	if err != nil {
		log.Println(err)
		return
	}

	client := &WebClient{
		Conn:    conn,
		Console: c,
		Send:    make(chan []byte),
		Route:   proxyUrl,
	}

	c.AddClient(proxyUrl, client)

	go client.Handle()
}

// HandleDisconnectClient handles the disconnection request for a SSH client.
func (c *WebConsole) HandleDisconnectClient(proxyUrl string, g *gin.Context) {
	client := strings.TrimPrefix(g.Request.URL.Path, "/_sish/api/disconnectclient/")

	c.State.SSHConnections.Range(func(key interface{}, val interface{}) bool {
		clientName := key.(string)

		if clientName == client {
			holderConn := val.(*SSHConnection)
			holderConn.CleanUp(c.State)

			return false
		}

		return true
	})

	data := map[string]interface{}{
		"status": true,
	}

	g.JSON(http.StatusOK, data)
}

// HandleDisconnectRoute handles the disconnection request for a forwarded route.
func (c *WebConsole) HandleDisconnectRoute(proxyUrl string, g *gin.Context) {
	route := strings.Split(strings.TrimPrefix(g.Request.URL.Path, "/_sish/api/disconnectroute/"), "/")
	encRouteName := route[1]

	decRouteName, err := base64.StdEncoding.DecodeString(encRouteName)
	if err != nil {
		log.Println("Error decoding route name:", err)
		err := g.AbortWithError(http.StatusInternalServerError, err)

		if err != nil {
			log.Println("Error aborting with error:", err)
		}
		return
	}

	routeName := string(decRouteName)

	listenerTmp, ok := c.State.Listeners.Load(routeName)
	if ok {
		listener, ok := listenerTmp.(*ListenerHolder)

		if ok {
			listener.Close()
		}
	}

	data := map[string]interface{}{
		"status": true,
	}

	g.JSON(http.StatusOK, data)
}

// HandleClients handles returning all connected SSH clients. This will
// also go through all of the forwarded connections for the SSH client and
// return them.
func (c *WebConsole) HandleClients(proxyUrl string, g *gin.Context) {
	data := map[string]interface{}{
		"status": true,
	}

	clients := map[string]map[string]interface{}{}
	c.State.SSHConnections.Range(func(key interface{}, val interface{}) bool {
		clientName := key.(string)
		sshConn := val.(*SSHConnection)

		listeners := []string{}
		routeListeners := map[string]map[string]interface{}{}

		sshConn.Listeners.Range(func(key interface{}, val interface{}) bool {
			name, ok := key.(string)

			if ok {
				listeners = append(listeners, name)
			}

			return true
		})

		tcpAliases := map[string]interface{}{}
		c.State.AliasListeners.Range(func(key interface{}, val interface{}) bool {
			tcpAlias := key.(string)
			aliasHolder := val.(*AliasHolder)

			for _, v := range listeners {
				for _, server := range aliasHolder.Balancer.Servers() {
					serverAddr, err := base64.StdEncoding.DecodeString(server.Host)
					if err != nil {
						log.Println("Error decoding server host:", err)
						continue
					}

					aliasAddress := string(serverAddr)

					if v == aliasAddress {
						tcpAliases[tcpAlias] = aliasAddress
					}
				}
			}

			return true
		})

		listenerParts := map[string]interface{}{}
		c.State.TCPListeners.Range(func(key interface{}, val interface{}) bool {
			tcpAlias := key.(string)
			aliasHolder := val.(*TCPHolder)

			for _, v := range listeners {
				aliasHolder.Balancers.Range(func(ikey, ival interface{}) bool {
					balancer := ival.(*roundrobin.RoundRobin)

					if aliasHolder.SNIProxy {
						tcpAlias = fmt.Sprintf("%s-%s", tcpAlias, ikey.(string))
					}

					for _, server := range balancer.Servers() {
						serverAddr, err := base64.StdEncoding.DecodeString(server.Host)
						if err != nil {
							log.Println("Error decoding server host:", err)
							continue
						}

						aliasAddress := string(serverAddr)

						if v == aliasAddress {
							listenerParts[tcpAlias] = aliasAddress
						}
					}

					return true
				})
			}

			return true
		})

		httpListeners := map[string]interface{}{}
		c.State.HTTPListeners.Range(func(key interface{}, val interface{}) bool {
			httpHolder := val.(*HTTPHolder)

			listenerHandlers := []string{}
			httpHolder.SSHConnections.Range(func(key interface{}, val interface{}) bool {
				httpAddr := key.(string)

				for _, v := range listeners {
					if v == httpAddr {
						listenerHandlers = append(listenerHandlers, httpAddr)
					}
				}
				return true
			})

			if len(listenerHandlers) > 0 {
				var userPass string
				password, _ := httpHolder.HTTPUrl.User.Password()
				if httpHolder.HTTPUrl.User.Username() != "" || password != "" {
					userPass = fmt.Sprintf("%s:%s@", httpHolder.HTTPUrl.User.Username(), password)
				}

				httpListeners[fmt.Sprintf("%s%s%s", userPass, httpHolder.HTTPUrl.Hostname(), httpHolder.HTTPUrl.Path)] = listenerHandlers
			}

			return true
		})

		routeListeners["tcpAliases"] = tcpAliases
		routeListeners["listeners"] = listenerParts
		routeListeners["httpListeners"] = httpListeners

		pubKey := ""
		pubKeyFingerprint := ""
		if sshConn.SSHConn.Permissions != nil {
			if _, ok := sshConn.SSHConn.Permissions.Extensions["pubKey"]; ok {
				pubKey = sshConn.SSHConn.Permissions.Extensions["pubKey"]
				pubKeyFingerprint = sshConn.SSHConn.Permissions.Extensions["pubKeyFingerprint"]
			}
		}

		clients[clientName] = map[string]interface{}{
			"remoteAddr":        sshConn.SSHConn.RemoteAddr().String(),
			"user":              sshConn.SSHConn.User(),
			"version":           string(sshConn.SSHConn.ClientVersion()),
			"session":           sshConn.SSHConn.SessionID(),
			"pubKey":            pubKey,
			"pubKeyFingerprint": pubKeyFingerprint,
			"listeners":         listeners,
			"routeListeners":    routeListeners,
		}

		return true
	})

	data["clients"] = clients

	g.JSON(http.StatusOK, data)
}

// RouteToken returns the route token for a specific route.
func (c *WebConsole) RouteToken(route string) (string, bool) {
	token, ok := c.RouteTokens.Load(route)
	routeToken := ""

	if ok {
		routeToken = token.(string)
	}

	return routeToken, ok
}

// RouteExists check if a route token exists.
func (c *WebConsole) RouteExists(route string) bool {
	_, ok := c.RouteToken(route)
	return ok
}

// AddRoute adds a route token to the console.
func (c *WebConsole) AddRoute(route string, token string) {
	c.Clients.LoadOrStore(route, []*WebClient{})
	c.RouteTokens.Store(route, token)
}

// RemoveRoute removes a route token from the console.
func (c *WebConsole) RemoveRoute(route string) {
	data, ok := c.Clients.Load(route)

	if !ok {
		return
	}

	clients, ok := data.([]*WebClient)

	if !ok {
		return
	}

	for _, client := range clients {
		client.Conn.Close()
	}

	c.Clients.Delete(route)
	c.RouteTokens.Delete(route)
}

// AddClient adds a client to the console route.
func (c *WebConsole) AddClient(route string, w *WebClient) {
	data, ok := c.Clients.Load(route)

	if !ok {
		return
	}

	clients, ok := data.([]*WebClient)

	if !ok {
		return
	}

	clients = append(clients, w)

	c.Clients.Store(route, clients)
}

// RemoveClient removes a client from the console route.
func (c *WebConsole) RemoveClient(route string, w *WebClient) {
	data, ok := c.Clients.Load(route)

	if !ok {
		return
	}

	clients, ok := data.([]*WebClient)

	if !ok {
		return
	}

	found := false
	toRemove := 0
	for i, client := range clients {
		if client == w {
			found = true
			toRemove = i
			break
		}
	}

	if found {
		clients[toRemove] = clients[len(clients)-1]
		c.Clients.Store(route, clients[:len(clients)-1])
	}
}

// BroadcastRoute sends a message to all clients on a route.
func (c *WebConsole) BroadcastRoute(route string, message []byte) {
	data, ok := c.Clients.Load(route)

	if !ok {
		return
	}

	clients, ok := data.([]*WebClient)

	if !ok {
		return
	}

	for _, client := range clients {
		client.Send <- message
	}
}

// Handle is the only place socket reads and writes happen.
func (c *WebClient) Handle() {
	defer func() {
		c.Conn.Close()
		c.Console.RemoveClient(c.Route, c)
	}()

	for message := range c.Send {
		w, err := c.Conn.NextWriter(websocket.TextMessage)
		if err != nil {
			return
		}

		_, err = w.Write(message)
		if err != nil {
			return
		}

		if err := w.Close(); err != nil {
			return
		}
	}

	err := c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
	if err != nil {
		log.Println("Error writing to websocket:", err)
	}
}
