package voice

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/polly"
	"github.com/bwmarrin/discordgo"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/vivisrc/say"
	"layeh.com/gopus"
)

const (
	audioSampleRate    = 48000
	audioChannels      = 2
	audioOpusFrameSize = 960
	audioOpusDataWidth = 2
	audioRawFrameSize  = audioOpusFrameSize * audioChannels
	audioOpusMaxBytes  = audioRawFrameSize * audioOpusDataWidth
)

const (
	idleTimeout = 10 * time.Minute
)

type voiceModule struct {
	*sync.RWMutex
	Bot         *say.Bot
	OpusEncoder *gopus.Encoder
	Sessions    map[string]*voiceSession
	Polly       *polly.Polly
	Voices      map[string]*polly.Voice
}

type voiceSession struct {
	*sync.Mutex
	Module     *voiceModule
	Connection *discordgo.VoiceConnection
	Alive      chan<- bool
}

func Install(bot *say.Bot) error {
	enc, err := gopus.NewEncoder(audioSampleRate, audioChannels, gopus.Voip)
	if err != nil {
		return err
	}

	mod := &voiceModule{
		RWMutex:     new(sync.RWMutex),
		Bot:         bot,
		OpusEncoder: enc,
		Sessions:    make(map[string]*voiceSession),
		Polly:       polly.New(bot.Aws),
		Voices:      make(map[string]*polly.Voice),
	}

	mod.Bot.InstallCommand(voiceCommand{
		voiceModule: mod,
	})
	mod.Bot.InstallCommand(prefixCommand{
		voiceModule: mod,
	})
	mod.Bot.InstallCommand(leaveCommand{
		voiceModule: mod,
	})

	var nextToken *string = nil
	engine := "standard"
	for {
		voices, err := mod.Polly.DescribeVoices(&polly.DescribeVoicesInput{
			Engine:    &engine,
			NextToken: nextToken,
		})
		if err != nil {
			return err
		}

		for _, voice := range voices.Voices {
			mod.Voices[*voice.Id+"#"+*voice.LanguageCode] = voice
		}

		nextToken = voices.NextToken
		if nextToken == nil {
			break
		}
	}

	mod.Bot.AddHandler(func(_ *discordgo.Session, message *discordgo.MessageCreate) {
		if message.Author.Bot {
			return
		}

		settings, err := mod.Bot.GetUser(message.Author.ID)
		if err != nil {
			log.Printf("error getting settings for user: %s", err)
			return
		}

		if !strings.HasPrefix(message.Content, settings.Prefix+" ") {
			return
		}

		content := strings.TrimSpace(message.Content[len(settings.Prefix):])
		if err = mod.HandleSay(content, settings, message.GuildID, message.Author.ID); err != nil {
			bot.ChannelMessageSendComplex(message.ChannelID, &discordgo.MessageSend{
				Content: err.Error(),
				AllowedMentions: &discordgo.MessageAllowedMentions{
					Parse:       []discordgo.AllowedMentionType{},
					RepliedUser: false,
				},
				Reference: &discordgo.MessageReference{
					MessageID: message.ID,
				},
			})
		}
	})

	mod.Bot.AddHandler(func(_ *discordgo.Session, voiceState *discordgo.VoiceStateUpdate) {
		if voiceState.UserID != bot.State.User.ID {
			return
		}

		if session, ok := mod.Sessions[voiceState.GuildID]; ok {
			session.Close()
		}
	})

	return nil
}

func (mod voiceModule) HandleSay(content string, user *say.User, guildID string, userID string) error {
	guild, err := mod.Bot.State.Guild(guildID)
	if err != nil {
		return err
	}

	for _, state := range guild.VoiceStates {
		if state.UserID == userID {
			session, err := mod.GetSession(guildID, state.ChannelID)
			if err != nil {
				return err
			}

			session.Lock()
			defer session.Unlock()

			if err := session.SayMessage(content, user); err != nil {
				return fmt.Errorf("internal error: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("you aren't in a voice channel")
}

func (mod voiceModule) GetSession(guildID string, channelID string) (*voiceSession, error) {
	mod.RLock()
	session, ok := mod.Sessions[guildID]
	mod.RUnlock()
	if ok {
		if session.Connection.ChannelID != channelID {
			return nil, fmt.Errorf("already in another voice channel")
		}

		session.Alive <- true
		return session, nil
	}

	mod.Lock()
	defer mod.Unlock()

	connection, ok := mod.Bot.VoiceConnections[guildID]
	if !ok {
		new, err := mod.Bot.ChannelVoiceJoin(guildID, channelID, false, true)
		if err != nil {
			go new.Disconnect()
			return nil, err
		}
		connection = new
	}

	alive := make(chan bool)
	session = &voiceSession{
		Mutex:      new(sync.Mutex),
		Module:     &mod,
		Connection: connection,
		Alive:      alive,
	}

	go func() {
		for {
			select {
			case <-alive:
			case <-time.After(idleTimeout):
				session.Close()
				return
			}
		}
	}()

	mod.Sessions[guildID] = session

	return session, nil
}

func (session voiceSession) SayMessage(message string, user *say.User) error {
	textType := "text"
	if strings.Contains(message, "<speak>") {
		textType = "ssml"
	}

	engine := "standard"
	outputFormat := "ogg_vorbis"

	out, err := session.Module.Polly.SynthesizeSpeech(&polly.SynthesizeSpeechInput{
		Engine:       &engine,
		OutputFormat: &outputFormat,
		Text:         &message,
		TextType:     &textType,
		VoiceId:      &user.Voice,
		LanguageCode: &user.VoiceLang,
	})
	if err != nil {
		return fmt.Errorf("unable to synthesize speech: %w", err)
	}

	ffmpeg := exec.Command(
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "warning",
		"-analyzeduration", "0",
		"-channel_layout", "mono",
		"-guess_layout_max", "0",
		"-c:a", "libvorbis",
		"-i", "pipe:0",
		"-f", "s16le",
		"-ar", "48000",
		"-ac", "2",
		"pipe:1",
	)

	ffmpeg.Stdin = out.AudioStream
	ffmpeg.Stderr = os.Stderr

	rawPcm, err := ffmpeg.StdoutPipe()
	if err != nil {
		return fmt.Errorf("couldn't create pipe to transcoder: %w", err)
	}

	err = ffmpeg.Start()
	if err != nil {
		return fmt.Errorf("couldn't start transcoder: %w", err)
	}

	for {
		pcm := make([]int16, audioRawFrameSize)
		err = binary.Read(rawPcm, binary.LittleEndian, &pcm)

		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("couldn't read raw audio: %w", err)
		}

		opus, err := session.Module.OpusEncoder.Encode(pcm, audioOpusFrameSize, audioOpusMaxBytes)
		if err != nil {
			return fmt.Errorf("couldn't encode raw audio: %w", err)
		}

		session.Connection.OpusSend <- opus
		session.Alive <- true
	}
}

func (session voiceSession) Close() {
	session.Lock()
	defer session.Unlock()

	delete(session.Module.Sessions, session.Connection.GuildID)
	session.Connection.Disconnect()
}

type voiceCommand struct {
	*voiceModule
}

func (voiceCommand) Data() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "voice",
		Description: "Sets the voice you talk with",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:         discordgo.ApplicationCommandOptionString,
				Name:         "name",
				Description:  "The name of the voice",
				Required:     true,
				Autocomplete: true,
			},
		},
	}
}

func (command voiceCommand) Handle(bot *say.Bot, interaction *discordgo.InteractionCreate) {
	data := interaction.ApplicationCommandData()

	voiceQuery := ""
	for _, option := range data.Options {
		if option.Name == "name" {
			voiceQuery = option.StringValue()
		}
	}

	if interaction.Type == discordgo.InteractionApplicationCommandAutocomplete {
		voiceStringToIdMap := make(map[string]string, len(command.Voices))
		voiceStrings := make([]string, 0, len(command.Voices))
		for id, voice := range command.Voices {
			s := fmt.Sprintf("%s (%s, %s)", *voice.Name, *voice.LanguageName, *voice.Gender)
			voiceStringToIdMap[s] = id
			voiceStrings = append(voiceStrings, s)
		}

		ranks := fuzzy.RankFindNormalizedFold(voiceQuery, voiceStrings)
		sort.Slice(ranks, ranks.Less)
		if len(ranks) > 25 {
			ranks = ranks[:25]
		}

		choices := make([]*discordgo.ApplicationCommandOptionChoice, len(ranks))
		for i, rank := range ranks {
			choices[i] = &discordgo.ApplicationCommandOptionChoice{
				Name:  rank.Target,
				Value: voiceStringToIdMap[rank.Target],
			}
		}

		bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionApplicationCommandAutocompleteResult,
			Data: &discordgo.InteractionResponseData{
				Choices: choices,
			},
		})
		return
	}

	user := interaction.User
	if user == nil {
		user = interaction.Member.User
	}

	settings, err := bot.GetUser(user.ID)

	if err != nil {
		log.Printf("error getting settings for user: %s", err)
		return
	}

	if voice, ok := command.Voices[voiceQuery]; ok {
		settings.Voice = *voice.Id
		settings.VoiceLang = *voice.LanguageCode
		bot.SaveUser(settings)

		bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Your voice has been updated",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	} else {
		bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "I couldn't find that voice",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}
}

type prefixCommand struct {
	*voiceModule
}

func (prefixCommand) Data() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Type:        discordgo.ChatApplicationCommand,
		Name:        "prefix",
		Description: "Sets the prefix to trigger TTS",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "value",
				Description: "The text string to use as a prefix",
				Required:    true,
			},
		},
	}
}

func (command prefixCommand) Handle(bot *say.Bot, interaction *discordgo.InteractionCreate) {
	data := interaction.ApplicationCommandData()

	value := ""
	for _, option := range data.Options {
		if option.Name == "value" {
			value = option.StringValue()
		}
	}

	user := interaction.User
	if user == nil {
		user = interaction.Member.User
	}

	settings, err := bot.GetUser(user.ID)

	if err != nil {
		log.Printf("error getting settings for user: %s", err)
		return
	}

	settings.Prefix = value
	bot.SaveUser(settings)

	bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "Your prefix has been updated",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

type leaveCommand struct {
	*voiceModule
}

func (leaveCommand) Data() *discordgo.ApplicationCommand {
	no := false

	return &discordgo.ApplicationCommand{
		Type:         discordgo.ChatApplicationCommand,
		Name:         "leave",
		Description:  "Leave the current voice chat",
		DMPermission: &no,
	}
}

func (command leaveCommand) Handle(bot *say.Bot, interaction *discordgo.InteractionCreate) {
	session, ok := command.Sessions[interaction.GuildID]
	if !ok {
		bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "I'm not in any voice channel",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	guild, err := bot.State.Guild(interaction.GuildID)
	if err != nil {
		return
	}

	for _, state := range guild.VoiceStates {
		if state.UserID == interaction.Member.User.ID {
			if session.Connection.ChannelID != state.ChannelID {
				break
			}

			session.Close()

			if err != nil {
				bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: fmt.Sprintf("I had a problem disconnecting: %s", err),
						Flags:   discordgo.MessageFlagsEphemeral,
					},
				})
			} else {
				bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "Successfully disconnected!",
					},
				})
			}

			return
		}
	}

	bot.InteractionRespond(interaction.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "You need to be in my voice channel to make me leave",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}
