package apns

import (
	"container/list"
	"crypto/tls"
	"io"
	"log"
	"time"
)

const (
	defBufferSiZe = 50
)

// buffer is a list of the last N push notification messages
type buffer struct {
	size int
	*list.List
}

// newBuffer return a buffer of the last <size> notification
func newBuffer(size int) *buffer {
	return &buffer{size, list.New()}
}

// Add pushes a Notification into the buffer
func (b *buffer) Add(v interface{}) *list.Element {
	e := b.PushBack(v)

	if b.Len() > b.size {
		b.Remove(b.Front())
	}

	return e
}

// Client maintains a connection to APNS server & is used to send notifications
type Client struct {
	// Conn is the underlying connection to APNS
	Conn         *Conn
	// FailedNotifs is a channel for receiving failed notifications
	FailedNotifs chan NotificationResult

	// incoming notification send request
	notifs chan Notification
	// id of the current notification request
	id     uint32
}

func newClientWithConn(gw string, conn Conn) Client {
	c := Client{
		Conn:         &conn,
		FailedNotifs: make(chan NotificationResult),
		id:           uint32(1),
		notifs:       make(chan Notification),
	}

	go c.runLoop()

	return c
}

// NewClientWithCert returns a new APNS client, given a TLS certificate
func NewClientWithCert(gw string, cert tls.Certificate) Client {
	conn := NewConnWithCert(gw, cert)

	return newClientWithConn(gw, conn)
}

// NewClient returns a new APNS client given a TLS cert & key (in plain text)
func NewClient(gw string, cert string, key string) (Client, error) {
	conn, err := NewConn(gw, cert, key)
	if err != nil {
		return Client{}, err
	}

	return newClientWithConn(gw, conn), nil
}

// NewClientWithFiles returns a new APNS client given a TLS cert & key files
func NewClientWithFiles(gw string, certFile string, keyFile string) (Client, error) {
	conn, err := NewConnWithFiles(gw, certFile, keyFile)
	if err != nil {
		return Client{}, err
	}

	return newClientWithConn(gw, conn), nil
}

// Send gets the client to send a push notification
func (c *Client) Send(n Notification) error {
	c.notifs <- n
	return nil
}

// reportFailedPush is called whenever we found a failed notification in buffer
// it will push the failed notification to the client's FailedNotifs
func (c *Client) reportFailedPush(v interface{}, err *Error) {
	failedNotif, ok := v.(Notification)
	if !ok || v == nil {
		return
	}

	select {
	case c.FailedNotifs <- NotificationResult{Notif: failedNotif, Err: *err}:
	default:
	}
}


// requeue puts notifications that appear after the cursor back into work queue
func (c *Client) requeue(cursor *list.Element) {
	// If `cursor` is not nil, this means there are notifications that
	// need to be delivered (or redelivered)
	for ; cursor != nil; cursor = cursor.Next() {
		if n, ok := cursor.Value.(Notification); ok {
			go func() { c.notifs <- n }()
		}
	}
}

// handleError is called whenever we found an error for failed push notification
// we will report the error and return the cursor (read pointer to list item) to it
func (c *Client) handleError(err *Error, buffer *buffer) *list.Element {
	cursor := buffer.Back()

	for cursor != nil {
		// Get notification
		n, _ := cursor.Value.(Notification)

		// If the notification, move cursor after the trouble notification
		if n.Identifier == err.Identifier {
			go c.reportFailedPush(cursor.Value, err)

			next := cursor.Next()

			buffer.Remove(cursor)
			return next
		}

		cursor = cursor.Prev()
	}

	return cursor
}

// runLoop is a goroutine that will continiously run with a client
// it will list for new Send request, as well as failed notification
func (c *Client) runLoop() {
	sent := newBuffer(defBufferSiZe)
	cursor := sent.Front()

	// APNS connection
	for {
		err := c.Conn.Connect()
		if err != nil {
			// TODO Probably want to exponentially backoff...
			time.Sleep(1 * time.Second)
			continue
		}

		// Start reading errors from APNS
		errs := readErrs(c.Conn)

		c.requeue(cursor)

		// Connection open, listen for notifs and errors
		for {
			var err error
			var n Notification

			// Check for notifications or errors. There is a chance we'll send notifications
			// if we already have an error since `select` will "pseudorandomly" choose a
			// ready channels. It turns out to be fine because the connection will already
			// be closed and it'll requeue. We could check before we get to this select
			// block, but it doesn't seem worth the extra code and complexity.
			select {
			case err = <-errs:
			case n = <-c.notifs:
			}

			// If there is an error we understand, find the notification that failed,
			// move the cursor right after it.
			if nErr, ok := err.(*Error); ok {
				cursor = c.handleError(nErr, sent)
				break
			}

			if err != nil {
				break
			}

			// Add to list
			cursor = sent.Add(n)

			// Set identifier if not specified
			if n.Identifier == 0 {
				n.Identifier = c.id // c.id belongs to the current notification
				c.id++              // no race-condition due to single-thread access
			} else if c.id < n.Identifier {
				c.id = n.Identifier + 1 // next notification
			}

			b, err := n.ToBinary()
			if err != nil {
				// TODO
				continue
			}

			_, err = c.Conn.Write(b)

			if err == io.EOF {
				log.Println("EOF trying to write notification")
				break
			}

			if err != nil {
				log.Println("err writing to apns", err.Error())
				break
			}

			cursor = cursor.Next()
		}
	}
}

// readErrs  read the first 6 bytes from the connection & push an error to a channel
// if the bytes indicates an error
func readErrs(c *Conn) chan error {
	errs := make(chan error)

	go func() {
		p := make([]byte, 6, 6)
		_, err := c.Read(p)
		if err != nil {
			errs <- err
			return
		}

		e := NewError(p)
		errs <- &e
	}()

	return errs
}
