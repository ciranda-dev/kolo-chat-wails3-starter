package main

// The Kolo tutorial fills in Start and Send.
type ChatService struct{}

func (c *ChatService) Start() (string, error) { return "", nil }

func (c *ChatService) Send(text string) error { return nil }
