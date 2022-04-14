package autorole

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/botlabs-gg/yagpdb/analytics"
	"github.com/botlabs-gg/yagpdb/bot"
	"github.com/botlabs-gg/yagpdb/bot/eventsystem"
	"github.com/botlabs-gg/yagpdb/commands"
	"github.com/botlabs-gg/yagpdb/common"
	"github.com/botlabs-gg/yagpdb/common/pubsub"
	"github.com/botlabs-gg/yagpdb/common/scheduledevents2"
	scheduledEventsModels "github.com/botlabs-gg/yagpdb/common/scheduledevents2/models"
	"github.com/jonas747/dcmd/v4"
	"github.com/jonas747/discordgo/v2"
	"github.com/mediocregopher/radix/v3"
)

var _ bot.BotInitHandler = (*Plugin)(nil)
var _ bot.BotStopperHandler = (*Plugin)(nil)
var _ commands.CommandProvider = (*Plugin)(nil)

func (p *Plugin) AddCommands() {
	commands.AddRootCommands(p, roleCommands...)
}

type assignRoleEventdata struct {
	UserID int64
	RoleID int64 // currently unused
}

func (p *Plugin) BotInit() {
	eventsystem.AddHandlerAsyncLast(p, onMemberJoin, eventsystem.EventGuildMemberAdd)
	// eventsystem.AddHandlerAsyncLast(p, HandlePresenceUpdate, eventsystem.EventPresenceUpdate)
	eventsystem.AddHandlerAsyncLastLegacy(p, handleGuildChunk, eventsystem.EventGuildMembersChunk)
	eventsystem.AddHandlerFirst(p, handleGuildMemberUpdate, eventsystem.EventGuildMemberUpdate)

	scheduledevents2.RegisterHandler("autorole_assign_role", assignRoleEventdata{}, handleAssignRole)

	// go runDurationChecker()
}

func (p *Plugin) StopBot(wg *sync.WaitGroup) {
	wg.Done()
}

var roleCommands = []*commands.YAGCommand{
	{
		CmdCategory: commands.CategoryDebug,
		Name:        "Roledbg",
		Description: "Debug debug debug autorole assignment",
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			var processing int
			err := common.RedisPool.Do(radix.Cmd(&processing, "GET", KeyProcessing(parsed.GuildData.GS.ID)))
			return fmt.Sprintf("Processing %d users.", processing), err
		},
	},
}

// HandlePresenceUpdate makes sure the member with joined_at is available for the relevant guilds
// TODO: Figure out a solution that scales better
// func HandlePresenceUpdate(evt *eventsystem.EventData) (retry bool, err error) {
// 	p := evt.PresenceUpdate()
// 	if p.Status == discordgo.StatusOffline {
// 		return
// 	}

// 	gs := evt.GS

// 	gs.RLock()
// 	m := gs.Member(false, p.User.ID)
// 	if m != nil && m.MemberSet {
// 		gs.RUnlock()
// 		return false, nil
// 	}
// 	gs.RUnlock()

// 	config, err := GuildCacheGetGeneralConfig(gs)
// 	if err != nil {
// 		return true, errors.WithStackIf(err)
// 	}

// 	if !config.OnlyOnJoin && config.Role != 0 {
// 		go bot.GetMember(gs.ID, p.User.ID)
// 	}

// 	return false, nil
// }

func saveGeneral(guildID int64, config *GeneralConfig) {

	err := common.SetRedisJson(KeyGeneral(guildID), config)
	if err != nil {
		logger.WithError(err).Error("Failed saving autorole config")
	} else {
		pubsub.Publish("autorole_stop_processing", guildID, nil)
	}
}

// Function to assign autorole to the user, or to schedule an event to assign the autorole
// This function gets triggered in either of the following ways:
// 1. A user joined a guild with membership screening completed.
// 2. The user joined the guild previously, but has completed membership screening just now.
func assignRoleAfterScreening(config *GeneralConfig, evt *eventsystem.EventData, member *discordgo.Member) (retry bool, err error) {
	if config.Role == 0 || evt.GS.GetRole(config.Role) == nil {
		return
	}

	memberJoinedAt, _ := member.JoinedAt.Parse()

	memberDuration := time.Since(memberJoinedAt)
	configDuration := time.Duration(config.RequiredDuration) * time.Minute

	if (config.RequiredDuration < 1 || config.OnlyOnJoin || configDuration <= memberDuration) && config.CanAssignTo(member.Roles, memberJoinedAt) {
		_, retry, err = assignRole(config, member.GuildID, member.User.ID)
		return retry, err
	}

	if !config.OnlyOnJoin {
		err = scheduledevents2.ScheduleEvent("autorole_assign_role", member.GuildID,
			time.Now().Add(configDuration-memberDuration), &assignRoleEventdata{UserID: member.User.ID})
		return bot.CheckDiscordErrRetry(err), err
	}

	return
}

func onMemberJoin(evt *eventsystem.EventData) (retry bool, err error) {
	addEvt := evt.GuildMemberAdd()

	config, err := GuildCacheGetGeneralConfig(addEvt.GuildID)
	if err != nil {
		return true, errors.WithStackIf(err)
	}

	if config.AssignRoleAfterScreening && addEvt.Pending {
		// Return if Membership Screening is pending
		return
	}

	return assignRoleAfterScreening(config, evt, addEvt.Member)
}

func assignRole(config *GeneralConfig, guildID int64, targetID int64) (disabled bool, retry bool, err error) {
	analytics.RecordActiveUnit(guildID, &Plugin{}, "assigned_role")
	err = common.BotSession.GuildMemberRoleAdd(guildID, targetID, config.Role)
	if err != nil {
		switch code, _ := common.DiscordError(err); code {
		case discordgo.ErrCodeUnknownMember:
			logger.WithError(err).Error("Unknown member when trying to assign role")
		case discordgo.ErrCodeMissingPermissions, discordgo.ErrCodeMissingAccess, discordgo.ErrCodeUnknownRole:
			logger.WithError(err).Warn("disabling autorole from error")
			cop := *config
			cop.Role = 0
			saveGeneral(guildID, &cop)
			return true, false, nil
		default:
			return false, bot.CheckDiscordErrRetry(err), err
		}
	}

	return false, false, nil
}

func (conf *GeneralConfig) CanAssignTo(currentRoles []int64, joinedAt time.Time) bool {
	if time.Since(joinedAt) < time.Duration(conf.RequiredDuration)*time.Minute {
		return false
	}

	if len(conf.IgnoreRoles) < 1 && len(conf.RequiredRoles) < 1 {
		return true
	}

	for _, ignoreRole := range conf.IgnoreRoles {
		if common.ContainsInt64Slice(currentRoles, ignoreRole) {
			return false
		}
	}

	// If require roles are set up, make sure the member has one of them
	if len(conf.RequiredRoles) > 0 {
		for _, reqRole := range conf.RequiredRoles {
			if common.ContainsInt64Slice(currentRoles, reqRole) {
				return true
			}
		}
		return false
	}

	return true
}

func RedisKeyGuildChunkProecssing(gID int64) string {
	return "autorole_guild_chunk_processing:" + strconv.FormatInt(gID, 10)
}

func RedisKeyFullScanStatus(gID int64) string {
	return "autorole_full_scan_status:" + strconv.FormatInt(gID, 10)
}

func RedisKeyFullScanAutoroleMembers(gID int64) string {
	return "autorole_full_scan_autorole_members:" + strconv.FormatInt(gID, 10)
}

func RedisKeyFullScanAssignedRoles(gID int64) string {
	return "autorole_full_scan_assigned_roles:" + strconv.FormatInt(gID, 10)
}

func handleGuildChunk(evt *eventsystem.EventData) {
	chunk := evt.GuildMembersChunk()
	guildID := chunk.GuildID
	if chunk.Nonce == "" || strconv.Itoa(int(guildID)) != chunk.Nonce {
		// This event was not triggered by Full Scan
		return
	}
	logger.Info(fmt.Sprintf("Total chunks: %d. Processing %d members in %s for index %d", chunk.ChunkCount, len(chunk.Members), chunk.Nonce, chunk.ChunkIndex))

	config, err := GetGeneralConfig(guildID)
	if err != nil {
		return
	}

	if config.Role == 0 || config.OnlyOnJoin {
		return
	}
	go iterateGuildChunkMembers(guildID, config, chunk)
}

// Iterate through all the members in the chunk, and add them to set, if autorole needs to be assigned to them
func iterateGuildChunkMembers(guildID int64, config *GeneralConfig, chunk *discordgo.GuildMembersChunk) {
	lastTimeFullScanStatusRefreshed := time.Now()
	err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanStatus(chunk.GuildID), "100", strconv.Itoa(FullScanIterating)))
	if err != nil {
		logger.WithError(err).Error("Failed marking full scan iterating")
	}

	for _, m := range chunk.Members {

		if config.AssignRoleAfterScreening && m.Pending {
			// Skip if Membership Screening is pending this member
			continue
		}

		joinedAt, err := m.JoinedAt.Parse()
		if err != nil {
			logger.WithError(err).WithField("ts", m.JoinedAt).WithField("user", m.User.ID).WithField("guild", guildID).Error("failed parsing join timestamp")
			if config.RequiredDuration > 0 {
				continue // Need the joined_at field for this
			}
		}

		if !config.CanAssignTo(m.Roles, joinedAt) {
			continue
		}

		// already has role
		if common.ContainsInt64Slice(m.Roles, config.Role) {
			continue
		}

		err = common.RedisPool.Do(radix.Cmd(nil, "ZADD", RedisKeyFullScanAutoroleMembers(chunk.GuildID), "-1", strconv.FormatInt(m.User.ID, 10)))
		if err != nil {
			logger.WithError(err).Error("Failed adding user to the set")
		}

		if time.Since(lastTimeFullScanStatusRefreshed) > time.Second*50 {
			lastTimeFullScanStatusRefreshed = time.Now()
			err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanStatus(chunk.GuildID), "100", strconv.Itoa(FullScanIterating)))
			if err != nil {
				logger.WithError(err).Error("Failed refreshing full scan iterating")
			}
		}
	}

	if chunk.ChunkIndex+1 == chunk.ChunkCount {
		// All chunks are processed, launching a go routine to start assigning autorole to the members in the set
		err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanStatus(chunk.GuildID), "10", strconv.Itoa(FullScanIterationDone)))
		if err != nil {
			logger.WithError(err).Error("Failed marking Full scan iteration complete")
		}
		logger.WithField("guild", guildID).Info("Full scan iteration is done, starting assigning roles.")
		go assignFullScanAutorole(guildID, config)
	}
}

// Fetches 10 member ids from the set and assigns autorole to them
func handleAssignFullScanRole(guildID int64, config *GeneralConfig, rolesAssigned *int, totalMembers int) bool {
	var uIDs []string
	common.RedisPool.Do(radix.Cmd(&uIDs, "ZPOPMIN", RedisKeyFullScanAutoroleMembers(guildID), "10"))
	uIDCount := len(uIDs)
	if uIDCount == 0 {
		return true
	}

	uIDsParsed := make([]int64, 0, uIDCount/2)
	for _, v := range uIDs {
		parsed, _ := strconv.ParseInt(v, 10, 64)
		if parsed < 0 {
			continue
		}
		uIDsParsed = append(uIDsParsed, parsed)
	}

	memberStates, _ := bot.GetMembers(guildID, uIDsParsed...)
	for _, ms := range memberStates {
		disabled, _, err := assignRole(config, guildID, ms.User.ID)
		if err != nil {
			logger.WithError(err).WithField("user", ms.User.ID).WithField("guild", guildID).Error("failed adding autorole role")
		}
		if disabled {
			return true
		}
		*rolesAssigned += 1
	}
	err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanAssignedRoles(guildID), "100", fmt.Sprintf("%d out of %d", *rolesAssigned, totalMembers)))
	if err != nil {
		logger.WithError(err).Error("Failed setting roles assigned count")
	}
	return false
}

func assignFullScanAutorole(guildID int64, config *GeneralConfig) {
	lastTimeFullScanStatusRefreshed := time.Now()
	err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanStatus(guildID), "100", strconv.Itoa(FullScanAssigningRole)))
	if err != nil {
		logger.WithError(err).Error("Failed marking Full scan assigning role")
	}

	var totalMembers int
	err = common.RedisPool.Do(radix.Cmd(&totalMembers, "ZCOUNT", RedisKeyFullScanAutoroleMembers(guildID), "-inf", "+inf"))
	if err != nil {
		logger.WithError(err).Error("Failed getting count of total members")
	}

	rolesAssigned := 0
OUTER:
	for {
		select {
		case <-cancelFullScan:
			logger.WithField("guild", guildID).Info("Full scan is cancelled by the user.")
			break OUTER
		default:
			assignmentDone := handleAssignFullScanRole(guildID, config, &rolesAssigned, totalMembers)
			if assignmentDone {
				break OUTER
			}
		}
		// Sleep for 1 second to prevent hitting discord's rate limits
		time.Sleep(time.Second * 1)

		if time.Since(lastTimeFullScanStatusRefreshed) > time.Second*50 {
			lastTimeFullScanStatusRefreshed = time.Now()
			err := common.RedisPool.Do(radix.Cmd(nil, "SETEX", RedisKeyFullScanStatus(guildID), "100", strconv.Itoa(FullScanAssigningRole)))
			if err != nil {
				logger.WithError(err).Error("Failed refreshing Full scan assigning role")
			}
		}
	}
	logger.WithField("guild", guildID).Info("Autorole full scan completed")
	err = common.RedisPool.Do(radix.Cmd(nil, "DEL", RedisKeyFullScanStatus(guildID), RedisKeyFullScanAutoroleMembers(guildID), RedisKeyFullScanAssignedRoles(guildID)))
	if err != nil {
		logger.WithError(err).Error("Failed deleting the full scan related keys from redis")
	}
}

func WorkingOnFullScan(guildID int64) bool {
	var b bool
	err := common.RedisPool.Do(radix.Cmd(&b, "EXISTS", RedisKeyGuildChunkProecssing(guildID)))
	if err != nil {
		logger.WithError(err).WithField("guild", guildID).Error("failed checking WorkingOnFullScan")
		return false
	}

	return b
}

func GuildCacheGetGeneralConfig(guildID int64) (*GeneralConfig, error) {
	v, err := configCache.Get(guildID)
	if err != nil {
		return nil, err
	}

	return v.(*GeneralConfig), nil
}

func handleAssignRole(evt *scheduledEventsModels.ScheduledEvent, data interface{}) (retry bool, err error) {
	config, err := GetGeneralConfig(evt.GuildID)
	if err != nil {
		return true, nil
	}

	if config.Role == 0 || config.OnlyOnJoin {
		// settings changed after they joined
		return false, nil
	}

	dataCast := data.(*assignRoleEventdata)

	member, err := bot.GetMember(evt.GuildID, dataCast.UserID)
	if err != nil {
		if common.IsDiscordErr(err, discordgo.ErrCodeUnknownMember) {
			return false, nil
		}

		return bot.CheckDiscordErrRetry(err), err
	}

	if config.AssignRoleAfterScreening && member.Member.Pending {
		// Return if Membership Screening is pending
		return
	}

	parsedT, _ := member.Member.JoinedAt.Parse()
	memberDuration := time.Now().Sub(parsedT)
	configDuration := time.Duration(config.RequiredDuration) * time.Minute
	if memberDuration < configDuration {
		// settings may have been changed, re-schedule
		err = scheduledevents2.ScheduleEvent("autorole_assign_role", evt.GuildID,
			time.Now().Add(configDuration-memberDuration), &assignRoleEventdata{UserID: dataCast.UserID})
		return bot.CheckDiscordErrRetry(err), err
	}

	if !config.CanAssignTo(member.Member.Roles, parsedT) {
		// some other reason they can't get the role, such as whitelist or ignore roles
		return false, nil
	}

	go analytics.RecordActiveUnit(evt.GuildID, &Plugin{}, "assigned_role")

	_, retry, err = assignRole(config, evt.GuildID, dataCast.UserID)
	return retry, err
}

func handleGuildMemberUpdate(evt *eventsystem.EventData) (retry bool, err error) {
	update := evt.GuildMemberUpdate()

	config, err := GuildCacheGetGeneralConfig(update.GuildID)
	if err != nil {
		return true, errors.WithStackIf(err)
	}

	if config.AssignRoleAfterScreening {
		if update.Pending {
			// Return if Membership Screening is pending
			return
		}

		prevMemberState, err := bot.GetMember(update.GuildID, update.User.ID)
		if err != nil {
			if common.IsDiscordErr(err, discordgo.ErrCodeUnknownMember) {
				return false, nil
			}

			return bot.CheckDiscordErrRetry(err), err
		}
		if prevMemberState != nil && prevMemberState.Member.Pending && !update.Pending {
			// The user has completed membership screening just now
			return assignRoleAfterScreening(config, evt, update.Member)
		}
	}

	if config.Role == 0 || config.OnlyOnJoin || evt.GS.GetRole(config.Role) == nil {
		return false, nil
	}

	if common.ContainsInt64Slice(update.Member.Roles, config.Role) {
		return false, nil
	}

	if !config.CanAssignTo(update.Member.Roles, time.Time{}) {
		return false, nil
	}

	if config.RequiredDuration > 0 {
		// check the autorole duration
		ms, err := bot.GetMember(update.GuildID, update.User.ID)
		if err != nil {
			return bot.CheckDiscordErrRetry(err), errors.WithStackIf(err)
		}

		parsedT, _ := ms.Member.JoinedAt.Parse()
		if time.Since(parsedT) < time.Duration(config.RequiredDuration)*time.Minute {
			// haven't been a member long enough
			return false, nil
		}
	}

	go analytics.RecordActiveUnit(update.GuildID, &Plugin{}, "assigned_role")

	// if we branched here then all the checks passed and they should be assigned the role
	_, retry, err = assignRole(config, update.GuildID, update.User.ID)
	return retry, err
}
