package main

import (
	"crypto/sha1"
	"fmt"
	"hash"
	"io"
	"mysql"
	"net"
	"strings"
	"container/list"
	pos "position"
)

var AutoClientId int = 0

type Client struct {
	id int
	socket   net.Conn
	loggedIn bool
}

func NewClient(_socket net.Conn) *Client {
	AutoClientId++
	return &Client{id: AutoClientId, socket: _socket, loggedIn: false}
}

func (c *Client) HandleClient() {
	for {
		packet := NewPacket()
		var headerbuffer [2]uint8
		recv, err := io.ReadFull(c.socket, headerbuffer[0:])
		if err != nil || recv == 0 {
			fmt.Printf("Disconnected: %d\n", c.id)
			break
		}
		copy(packet.Buffer[0:2], headerbuffer[0:2])
		packet.GetHeader()

		databuffer := make([]uint8, packet.MsgSize)

		reloop := false
		bytesReceived := uint16(0)
		for bytesReceived < packet.MsgSize {
			recv, err = io.ReadFull(c.socket, databuffer[bytesReceived:])
			if recv == 0 {
				reloop = true
				break
			} else if err != nil {
				fmt.Printf("Connection read error: %v\n", err)
				reloop = true
				break
			}
			bytesReceived += uint16(recv)
		}
		if reloop {
			continue
		}

		copy(packet.Buffer[2:], databuffer[:])

		header := packet.ReadUint8()
		switch header {
		case 0x00: // Login
			username := packet.ReadString()
			password := packet.ReadString()
			if c.checkAccount(username, password) {
				c.loggedIn = true
				c.SendLogin(0)
				c.SendMapList()
			} else {
				c.SendLogin(1)
			}
			
		case 0x01: // Request map piece
			if c.loggedIn {
				x := int(packet.ReadInt16())
				y := int(packet.ReadInt16())
				z := int(packet.ReadUint16())
				w := int(packet.ReadUint16())
				h := int(packet.ReadUint16())

				c.SendArea(x, y, z, w + x, h + y)
			}

		case 0x02: // Tile changes
			go c.ReceiveChange(packet)
			
		case 0x03: // Request map list
			if c.loggedIn {
				c.SendMapList()
			}
			
		case 0x04: // Add map
			go c.ReceiveAddMap(packet)
			
		case 0x05: // Delete map
			go c.ReceiveRemoveMap(packet)
			
		case 0x06: // Update tile event
			go c.ReceiveTileEventUpdate(packet)
			
		default:
			fmt.Printf("Unknown header: %d", header)
			
		}
	}
	fmt.Printf("Client disconnected: %d\n", c.id)
}

func (c *Client) checkAccount(_username string, _password string) bool {
	var query string = fmt.Sprintf("SELECT * FROM mapchange_account WHERE username = '%s'", _username)
	var err error
	var result *mysql.Result
	if result, err = DBQuerySelect(query); err != nil {
		fmt.Printf("Query error: %s", err.Error())
		return false
	}
	
	defer result.Free()

	row := result.FetchMap()
	if row != nil {
		hashedpass := row["password"].(string)
		return c.passwordTest(_password, hashedpass)
	}
	return false
}

func (c *Client) passwordTest(_plain string, _hash string) bool {
	var h hash.Hash = sha1.New()
	h.Write([]byte(_plain))

	var sha1Hash string = strings.ToUpper(fmt.Sprintf("%x", h.Sum(nil)))
	var original string = strings.ToUpper(_hash)

	return (sha1Hash == original)
}

func (c *Client) ReceiveChange(_packet *Packet) {
	if !c.loggedIn {
		return
	}

	g_dblock.Lock()
	defer g_dblock.Unlock()

	numTiles := int(_packet.ReadUint16())
	if numTiles <= 0 { // Zero tile selected bug
		return
	}
	
	updatedTiles := list.New()
	
	for i := 0; i < numTiles; i++ {
		x := int(_packet.ReadInt16())
		y := int(_packet.ReadInt16())
		z := int(_packet.ReadUint16())
		movement := int(_packet.ReadUint16())
		numLayers := int(_packet.ReadUint16())
		
		// Check if tile already exists
		tile, exists := g_map.GetTileFromCoordinates(x, y, z)
		var query string
		
		if numLayers > 0 {
			if !exists { // Tile does not exists, create it
				query := fmt.Sprintf("INSERT INTO tile (x, y, z, movement, idlocation) VALUES (%d, %d, %d, %d, 0)", x, y, z, movement)
				if err := DBQuery(query); err != nil {
					fmt.Printf("Database query error: %s\n", err.Error())
					return
				}
				
				tile = NewTileExt(x, y, z)
				tile.Blocking = movement
				tile.DbId = int64(g_db.LastInsertId)
				
				// Add tile to map
				g_map.AddTile(tile)
			} else {
				query := fmt.Sprintf("UPDATE tile SET movement='%d' WHERE idtile='%d'", movement, tile.DbId)
				if err := DBQuery(query); err != nil {
					fmt.Printf("Database query error: %s\n", err.Error())
					return
				}
				
				tile.Blocking = movement
			}

			for j := 0; j < numLayers; j++ {
				layerId := int(_packet.ReadUint16())
				sprite := int(_packet.ReadUint16())
			
				tileLayer := tile.GetLayer(layerId)
				if tileLayer == nil {
					query = fmt.Sprintf("INSERT INTO tile_layer (idtile, layer, sprite) VALUES (%d, %d, %d)", tile.DbId, layerId, sprite)
					if err := DBQuery(query); err != nil {
						fmt.Printf("Database query error: %s\n", err.Error())
						return
					}
					
					tileLayer = tile.AddLayer(layerId, sprite)
					tileLayer.DbId = int64(g_db.LastInsertId)
				} else {
					if (sprite == 0) { // Delete layer
						query = fmt.Sprintf("DELETE FROM tile_layer WHERE idtile_layer='%d'", tileLayer.DbId)
						if err := DBQuery(query); err != nil {
							fmt.Printf("Database query error: %s\n", err.Error())
							return
						}
						
						tile.RemoveLayer(layerId)						
					} else {
						query = fmt.Sprintf("UPDATE tile_layer SET sprite='%d' WHERE idtile_layer='%d'", sprite, tileLayer.DbId)
						if err := DBQuery(query); err != nil {
							fmt.Printf("Database query error: %s\n", err.Error())
							return
						}
						
						tileLayer.SpriteID = sprite
					}
				}
			}
		} else {
			if exists {
				query = fmt.Sprintf("DELETE FROM tile_layer WHERE idtile='%d'", tile.DbId)
				if err := DBQuery(query); err != nil {
					fmt.Printf("Database query error: %s\n", err.Error())
					return
				}
				
				// Check if tile has an event 
				if tile.Event != nil {
					if tile.Event.GetEventType() == 1 { // Warp/Teleport
						warp := tile.Event.(*Warp)
						query := fmt.Sprintf("DELETE FROM teleport WHERE idteleport = %d", warp.dbid)
						if err := DBQuery(query); err == nil {
							fmt.Printf("Database query error: %s\n", err.Error())
						}
					}				
				}
				
				query = fmt.Sprintf("DELETE FROM tile WHERE idtile='%d'", tile.DbId)
				if err := DBQuery(query); err != nil {
					fmt.Printf("Database query error: %s\n", err.Error())
					return
				}
				
				tile.IsRemoved = true
			}
		}
		
		updatedTiles.PushBack(tile)
	}
	
	g_server.SendTileUpdateToClients(updatedTiles, c.id)
}

func (c *Client) ReceiveAddMap(_packet *Packet) {
	if !c.loggedIn {
		return
	}
	
	mapName := _packet.ReadString()
	if len(mapName) > 0 {
		g_dblock.Lock()
		defer g_dblock.Unlock()
		
		query := fmt.Sprintf("INSERT INTO map (name) VALUES ('%s')", mapName)
		if DBQuery(query) == nil {
			mapId := int(g_db.LastInsertId)
			g_map.AddMap(mapId, mapName)
			
			g_server.SendMapListUpdateToClients()
		}
	}
}

func (c *Client) ReceiveRemoveMap(_packet *Packet) {
	if !c.loggedIn {
		return
	}
	
	mapId := int(_packet.ReadUint16())
	
	// Check if map id exists
	if _, ok := g_map.GetMap(mapId); ok {	
		g_dblock.Lock()
		defer g_dblock.Unlock()	
	
		query := fmt.Sprintf("DELETE FROM map WHERE idmap='%d'", mapId)
		if DBQuery(query) == nil {
			g_map.DeleteMap(mapId)
			
			// Send map deleted to clients
			// TODO
			
			// Send new list to clients
			g_server.SendMapListUpdateToClients()
		}
	}
}

func (c *Client) ReceiveTileEventUpdate(_packet *Packet) {
	if !c.loggedIn {
		return;
	}
	
	x := int(_packet.ReadInt16())
	y := int(_packet.ReadInt16())
	z := int(_packet.ReadInt16())
	
	if tile, found := g_map.GetTileFromCoordinates(x, y, z); found {	
		eventType := int(_packet.ReadUint8())
		
		g_dblock.Lock()
		defer g_dblock.Unlock()
		
		if eventType == 0 { // Remove event
			if tile.Event != nil {
				if tile.Event.GetEventType() == 1 { // Warp/Teleport
					warp := tile.Event.(*Warp)
					query := fmt.Sprintf("DELETE FROM teleport WHERE idteleport = %d", warp.dbid)
					if err := DBQuery(query); err == nil {
						// Update tile
						query = fmt.Sprintf("UPDATE tile SET idteleport = 0 WHERE idtile = %d", tile.DbId)
						if updateErr := DBQuery(query); updateErr == nil {
							tile.Event = nil;
						}
					}
				}
			}
		} else if tile.Event != nil && tile.Event.GetEventType() == eventType { // Update
			if eventType == 1 {
				warp := tile.Event.(*Warp)
				toX := int(_packet.ReadInt16())
				toY := int(_packet.ReadInt16())
				toZ := int(_packet.ReadInt16())
				
				query := fmt.Sprintf("UPDATE teleport SET x = %d, y = %d, z = %d WHERE idteleport = %d", toX, toY, toZ, warp.dbid)
				if err := DBQuery(query); err == nil {
					warp.destination.X = toX
					warp.destination.Y = toY
					warp.destination.Z = toZ
				}
			}
		} else { // Add
			if eventType == 1 {
				toX := int(_packet.ReadInt16())
				toY := int(_packet.ReadInt16())
				toZ := int(_packet.ReadInt16())
				tp_pos := pos.NewPositionFrom(toX, toY, toZ)
				warp := NewWarp(tp_pos)
				
				query := fmt.Sprintf("INSERT INTO teleport (x, y, z) VALUES (%d, %d, %d)", toX, toY, toZ)
				if err := DBQuery(query); err == nil {
					warp.dbid = int64(g_db.LastInsertId)
					
					updateQuery := fmt.Sprintf("UPDATE tile SET idteleport = %d WHERE idtile = %d", warp.dbid, tile.DbId)
					if updateErr := DBQuery(updateQuery); updateErr == nil {
						tile.AddEvent(warp)
					}
				}
			}
		}
	}
}

// //////////////////////////////////////////////
// SEND
// //////////////////////////////////////////////

func (c *Client) SendLogin(_status int) {
	packet := NewPacketExt(0x00)
	packet.AddUint8(uint8(_status))
	c.Send(packet)
}

func (c *Client) SendArea(_x, _y, _z, _w, _h int) {
	packet := NewPacketExt(0x01)
	packet.AddUint16(0)
	packet.AddUint16(uint16(_z))
	count := 0
	for x := _x; x < _w; x++ {
		for y := _y; y < _h; y++ {
			if packet.MsgSize > 8000 {
				packet.MsgSize -= 2
				packet.readPos = 3
				packet.AddUint16(uint16(count))
				c.Send(packet)
				
				packet = NewPacketExt(0x01)
				packet.AddUint16(0)
				packet.AddUint16(uint16(_z))
				count = 0
			}
			tile, found := g_map.GetTileFromCoordinates(x, y, _z)
			if found == true {
				count++

				packet.AddUint16(uint16(x))
				packet.AddUint16(uint16(y))
				packet.AddUint8(uint8(tile.Blocking))
				
				if tile.Event != nil {
					packet.AddUint8(uint8(tile.Event.GetEventType()))
					if tile.Event.GetEventType() == 1 {
						warp := tile.Event.(*Warp)
						packet.AddUint16(uint16(warp.destination.X))
						packet.AddUint16(uint16(warp.destination.Y))
						packet.AddUint16(uint16(warp.destination.Z))
					}
				} else {
					packet.AddUint8(0)
				}
				
				packet.AddUint8(uint8(len(tile.Layers)))
				for _, layer := range tile.Layers {
					if layer != nil {
						packet.AddUint8(uint8(layer.Layer))
						packet.AddUint16(uint16(layer.SpriteID))
					}
				}
			}
		}
	}
	
	packet.MsgSize -= 2
	packet.readPos = 3
	packet.AddUint16(uint16(count))
	c.Send(packet)
}

func (c *Client) SendMapList() {
	packet := NewPacketExt(0x03)
	packet.AddUint16(uint16(len(g_map.mapNames)))
	
	for index, value := range(g_map.mapNames) {
		packet.AddUint16(uint16(index))
		packet.AddString(value)
	}
	
	c.Send(packet)
}

func (c *Client) Send(_packet *Packet) {
	_packet.SetHeader()
	c.socket.Write(_packet.Buffer[0:_packet.MsgSize])
}