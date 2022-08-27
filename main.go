package say

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/bwmarrin/discordgo"
	"github.com/go-rel/postgres"
	"github.com/go-rel/rel"
	"github.com/go-rel/rel/where"
	"github.com/jellydator/ttlcache/v3"
	_ "github.com/lib/pq"
)

const (
	cacheTTL = 30 * time.Minute
)

type Bot struct {
	*discordgo.Session
	Commands  map[string]Command
	Context   context.Context
	userCache *ttlcache.Cache[string, *User]
	Repo      rel.Repository
	Aws       *session.Session
}

func New() (*Bot, error) {
	discord, err := discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))
	if err != nil {
		return nil, err
	}

	aws, err := session.NewSession()
	if err != nil {
		return nil, err
	}

	bot := &Bot{
		Session:   discord,
		Commands:  make(map[string]Command),
		Context:   context.Background(),
		userCache: ttlcache.New[string, *User](),
		Repo:      nil,
		Aws:       aws,
	}

	bot.LogLevel = discordgo.LogInformational

	bot.Identify.Intents = 0
	bot.Identify.Intents |= discordgo.IntentGuilds
	bot.Identify.Intents |= discordgo.IntentGuildVoiceStates
	bot.Identify.Intents |= discordgo.IntentGuildMessages
	bot.Identify.Intents |= discordgo.IntentMessageContent

	bot.AddHandler(func(_ *discordgo.Session, ready *discordgo.Ready) {
		log.Printf(
			"Logged in as @%s#%s (%s)",
			ready.User.Username,
			ready.User.Discriminator,
			ready.User.ID,
		)
	})

	bot.AddHandler(func(_ *discordgo.Session, interaction *discordgo.InteractionCreate) {
		if interaction.Type != discordgo.InteractionApplicationCommand &&
			interaction.Type != discordgo.InteractionApplicationCommandAutocomplete {
			return
		}

		if command, ok := bot.Commands[interaction.ApplicationCommandData().Name]; ok {
			command.Handle(bot, interaction)
		}
	})

	return bot, nil
}

func (bot *Bot) RegisterCommands() error {
	self, err := bot.User("@me")
	if err != nil {
		return err
	}

	appCommands := make([]*discordgo.ApplicationCommand, 0, len(bot.Commands))
	for _, command := range bot.Commands {
		appCommands = append(appCommands, command.Data())
	}

	_, err = bot.ApplicationCommandBulkOverwrite(self.ID, "", appCommands)
	return err
}

func (bot *Bot) Open() error {
	adapter, err := postgres.Open(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}

	bot.Repo = rel.New(adapter)
	bot.Repo.Instrumentation(func(ctx context.Context, op string, message string) func(error) {
		start := time.Now()
		return func(err error) {
			duration := time.Since(start)
			if err != nil {
				log.Printf("[%s: %s] %s - %s", op, duration, message, err)
			} else {
				log.Printf("[%s: %s] %s", op, duration, message)
			}
		}
	})

	if err := bot.Session.Open(); err != nil {
		return err
	}

	return nil
}

func (bot *Bot) Close() error {
	if err := bot.Session.Close(); err != nil {
		return err
	}

	if err := bot.Repo.Adapter(context.Background()).Close(); err != nil {
		return err
	}

	return nil
}

type Command interface {
	Data() *discordgo.ApplicationCommand
	Handle(*Bot, *discordgo.InteractionCreate)
}

func (bot *Bot) InstallCommand(command Command) {
	bot.Commands[command.Data().Name] = command
}

type User struct {
	ID        uint64
	Voice     string
	VoiceLang string
	Prefix    string
}

func (bot *Bot) GetUser(userID string) (*User, error) {
	item := bot.userCache.Get(userID)
	if item != nil {
		return item.Value(), nil
	}

	id, err := strconv.ParseUint(userID, 10, 64)
	if err != nil {
		return nil, err
	}

	user := new(User)
	if err := bot.Repo.Find(bot.Context, user, where.Eq("id", id)); err != nil {
		if err != rel.ErrNotFound {
			return nil, err
		}

		user = &User{
			ID:        id,
			Voice:     "Joanna",
			VoiceLang: "en-US",
			Prefix:    "say",
		}
	}

	bot.userCache.Set(userID, user, cacheTTL)
	return user, nil
}

func (bot *Bot) SaveUser(user *User) {
	bot.userCache.Set(fmt.Sprint(user.ID), user, cacheTTL)

	bot.Repo.Insert(bot.Context, user, rel.OnConflictReplace())
}
