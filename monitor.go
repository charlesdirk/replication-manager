// monitor.go
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/tanji/mariadb-tools/dbhelper"
)

// ServerMonitor defines a server to monitor.
type ServerMonitor struct {
	Conn           *sqlx.DB
	URL            string
	Host           string
	Port           string
	IP             string
	BinlogPos      string
	Strict         string
	ServerID       uint
	MasterServerID uint
	MasterHost     string
	LogBin         string
	UsingGtid      string
	CurrentGtid    string
	SlaveGtid      string
	IOThread       string
	SQLThread      string
	ReadOnly       string
	Delay          sql.NullInt64
	State          string
	PrevState      string
}

type serverList []*ServerMonitor

/* Initializes a server object */
func newServerMonitor(url string) (*ServerMonitor, error) {
	server := new(ServerMonitor)
	server.URL = url
	server.Host, server.Port = splitHostPort(url)
	var err error
	server.IP, err = dbhelper.CheckHostAddr(server.Host)
	if err != nil {
		return server, errors.New(fmt.Sprintf("ERROR: DNS resolution error for host %s", server.Host))
	}
	params := fmt.Sprintf("timeout=%ds", timeout)
	server.Conn, err = dbhelper.MySQLConnect(dbUser, dbPass, dbhelper.GetAddress(server.Host, server.Port, socket), params)
	if err != nil {
		server.State = stateFailed
		return server, err
	}
	return server, nil
}

/* Refresh a server object */
func (server *ServerMonitor) refresh() error {
	err := server.Conn.Ping()
	if err != nil {
		// we want the failed state for masters to be set by the monitor
		if server.State != stateMaster {
			server.State = stateFailed
			// remove from slave list
			server.delete(&slaves)
		}
		return err
	}
	sv, err := dbhelper.GetVariables(server.Conn)
	if err != nil {
		return err
	}
	server.PrevState = server.State
	server.BinlogPos = sv["GTID_BINLOG_POS"]
	server.Strict = sv["GTID_STRICT_MODE"]
	server.LogBin = sv["LOG_BIN"]
	server.ReadOnly = sv["READ_ONLY"]
	server.CurrentGtid = sv["GTID_CURRENT_POS"]
	server.SlaveGtid = sv["GTID_SLAVE_POS"]
	sid, _ := strconv.ParseUint(sv["SERVER_ID"], 10, 0)
	server.ServerID = uint(sid)
	slaveStatus, err := dbhelper.GetSlaveStatus(server.Conn)
	if err != nil {
		// If we reached this stage with a previously failed server, reintroduce
		// it as unconnected server.
		if server.State == stateFailed {
			server.State = stateUnconn
			if autorejoin {
				if verbose {
					logprint("INFO : Rejoining previously failed server", server.URL)
				}
				err := server.rejoin()
				if err != nil {
					logprint("ERROR: Failed to autojoin previously failed server", server.URL)
				}
			}
		}
		return err
	}
	server.UsingGtid = slaveStatus.Using_Gtid
	server.IOThread = slaveStatus.Slave_IO_Running
	server.SQLThread = slaveStatus.Slave_SQL_Running
	server.Delay = slaveStatus.Seconds_Behind_Master
	server.MasterServerID = slaveStatus.Master_Server_Id
	server.MasterHost = slaveStatus.Master_Host
	// In case of state change, reintroduce the server in the slave list
	if server.PrevState == stateFailed || server.PrevState == stateUnconn {
		server.State = stateSlave
		slaves = append(slaves, server)
	}
	return err
}

/* Check replication health and return status string */
func (server *ServerMonitor) healthCheck() string {
	if server.Delay.Valid == false {
		if server.SQLThread == "Yes" && server.IOThread == "No" {
			return "NOT OK, IO Stopped"
		} else if server.SQLThread == "No" && server.IOThread == "Yes" {
			return "NOT OK, SQL Stopped"
		} else {
			return "NOT OK, ALL Stopped"
		}
	} else {
		if server.Delay.Int64 > 0 {
			return "Behind master"
		}
		return "Running OK"
	}
}

/* Handles write freeze and existing transactions on a server */
func (server *ServerMonitor) freeze() bool {
	err := dbhelper.SetReadOnly(server.Conn, true)
	if err != nil {
		logprintf("WARN : Could not set %s as read-only: %s", server.URL, err)
		return false
	}
	for i := waitKill; i > 0; i -= 500 {
		threads := dbhelper.CheckLongRunningWrites(server.Conn, 0)
		if threads == 0 {
			break
		}
		logprintf("INFO : Waiting for %d write threads to complete on %s", threads, server.URL)
		time.Sleep(500 * time.Millisecond)
	}
	logprintf("INFO : Terminating all threads on %s", server.URL)
	dbhelper.KillThreads(server.Conn)
	return true
}

/* Returns a candidate from a list of slaves. If there's only one slave it will be the de facto candidate. */
func (server *ServerMonitor) electCandidate(l []*ServerMonitor) int {
	ll := len(l)
	if verbose {
		logprintf("DEBUG: Processing %d candidates", ll)
	}
	seqList := make([]uint64, ll)
	i := 0
	hiseq := 0
	for _, sl := range l {
		if failover == "" {
			if verbose {
				logprintf("DEBUG: Checking eligibility of slave server %s", sl.URL)
			}
			if dbhelper.CheckSlavePrerequisites(sl.Conn, sl.Host) == false {
				continue
			}
			if dbhelper.CheckBinlogFilters(server.Conn, sl.Conn) == false {
				logprintf("WARN : Binlog filters differ on master and slave %s. Skipping", sl.URL)
				continue
			}
			if dbhelper.CheckReplicationFilters(server.Conn, sl.Conn) == false {
				logprintf("WARN : Replication filters differ on master and slave %s. Skipping", sl.URL)
				continue
			}
			ss, _ := dbhelper.GetSlaveStatus(sl.Conn)
			if ss.Seconds_Behind_Master.Valid == false {
				logprintf("WARN : Slave %s is stopped. Skipping", sl.URL)
				continue
			}
			if ss.Seconds_Behind_Master.Int64 > maxDelay {
				logprintf("WARN : Slave %s has more than %d seconds of replication delay (%d). Skipping", sl.URL, maxDelay, ss.Seconds_Behind_Master.Int64)
				continue
			}
			if gtidCheck && dbhelper.CheckSlaveSync(sl.Conn, server.Conn) == false {
				logprintf("WARN : Slave %s not in sync. Skipping", sl.URL)
				continue
			}
		}
		/* If server is in the ignore list, do not elect it */
		if contains(ignoreList, sl.URL) {
			if verbose {
				logprintf("DEBUG: %s is in the ignore list. Skipping", sl.URL)
			}
			continue
		}
		/* Rig the election if the examined slave is preferred candidate master */
		if sl.URL == prefMaster {
			if verbose {
				logprintf("DEBUG: Election rig: %s elected as preferred master", sl.URL)
			}
			return i
		}
		seqList[i] = getSeqFromGtid(dbhelper.GetVariableByName(sl.Conn, "GTID_CURRENT_POS"))
		var max uint64
		if i == 0 {
			max = seqList[0]
		} else if seqList[i] > max {
			max = seqList[i]
			hiseq = i
		}
		i++
	}
	if i > 0 {
		/* Return key of slave with the highest seqno. */
		return hiseq
	}
	logprint("ERROR: No suitable candidates found.")
	return -1
}

func (server *ServerMonitor) log() {
	server.refresh()
	logprintf("DEBUG: Server:%s Current GTID:%s Slave GTID:%s Binlog Pos:%s", server.URL, server.CurrentGtid, server.SlaveGtid, server.BinlogPos)
	return
}

func (server *ServerMonitor) writeState() error {
	server.log()
	f, err := os.Create("/tmp/repmgr.state")
	if err != nil {
		return err
	}
	_, err = f.WriteString(server.BinlogPos)
	if err != nil {
		return err
	}
	return nil
}

func (server *ServerMonitor) hasSiblings(sib []*ServerMonitor) bool {
	for _, sl := range sib {
		if server.MasterServerID != sl.MasterServerID {
			return false
		}
	}
	return true
}

func (server *ServerMonitor) delete(sl *serverList) {
	lsm := *sl
	for k, s := range lsm {
		if server.URL == s.URL {
			lsm[k] = lsm[len(lsm)-1]
			lsm[len(lsm)-1] = nil
			lsm = lsm[:len(lsm)-1]
			break
		}
	}
	*sl = lsm
}

func (server *ServerMonitor) rejoin() error {
	if readonly {
		dbhelper.SetReadOnly(server.Conn, true)
	}
	cm := "CHANGE MASTER TO master_host='" + master.IP + "', master_port=" + master.Port + ", master_user='" + rplUser + "', master_password='" + rplPass + "', MASTER_USE_GTID=CURRENT_POS"
	_, err := server.Conn.Exec(cm)
	dbhelper.StartSlave(server.Conn)
	return err
}
