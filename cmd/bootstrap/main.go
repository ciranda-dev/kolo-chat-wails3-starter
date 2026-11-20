package main

import (
	"context"
	"log"
	"os"
	"os/signal"

	kolodht "github.com/ciranda-dev/kolo-dht"
	koloid "github.com/ciranda-dev/kolo-id"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	id, err := koloid.Generate()
	if err != nil {
		log.Fatal(err)
	}
	s, err := kolodht.New(ctx, "127.0.0.1:4000", id, &kolodht.Config{Server: true})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Println("bootstrap node on 127.0.0.1:4000")
	<-ctx.Done()
}
