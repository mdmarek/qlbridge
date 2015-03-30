package exec

import (
	"fmt"
	"net/url"
	"sync"
	//"time"

	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/value"
	"github.com/araddon/qlbridge/vm"
	//"github.com/mdmarek/topo"
)

var (
	_ = u.EMPTY

	// Ensure that we implement the Task Runner interface
	// to ensure this can run in exec engine
	_ TaskRunner = (*Source)(nil)
)

// Scan a data source for rows, feed into runner.  The source scanner being
//   a source is iter.Next() messages instead of sending them on input channel
//
//  1) table      -- FROM table
//  2) channels   -- FROM stream
//  3) join       -- SELECT t1.name, t2.salary
//                       FROM employee AS t1
//                       INNER JOIN info AS t2
//                       ON t1.name = t2.name;
//  4) sub-select -- SELECT * FROM (SELECT 1, 2, 3) AS t1;
//
type Source struct {
	*TaskBase
	source datasource.Scanner
}

// A scanner to read from data source
func NewSource(from string, source datasource.Scanner) *Source {
	s := &Source{
		TaskBase: NewTaskBase("Source"),
		source:   source,
	}
	s.TaskBase.TaskType = s.Type()

	return s
}

func (m *Source) Copy() *Source { return &Source{} }

func (m *Source) Close() error {
	if closer, ok := m.source.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func (m *Source) Run(context *Context) error {
	defer context.Recover() // Our context can recover panics, save error msg
	defer close(m.msgOutCh) // closing input channels is the signal to stop

	// TODO:  Allow an alternate interface that allows Source to provide
	//        an output channel?
	scanner, ok := m.source.(datasource.Scanner)
	if !ok {
		return fmt.Errorf("Does not implement Scanner: %T", m.source)
	}
	iter := scanner.CreateIterator(nil)

	for item := iter.Next(); item != nil; item = iter.Next() {

		//u.Infof("In source Scanner iter %#v", item)
		select {
		case <-m.SigChan():
			u.Warnf("got signal quit")
			return nil
		case m.msgOutCh <- item:
			// continue
		}

	}
	//u.Debugf("leaving source scanner")
	return nil
}

// Scan a data source for rows, feed into runner for join sources
//
//  1) join  SELECT t1.name, t2.salary
//               FROM employee AS t1
//               INNER JOIN info AS t2
//               ON t1.name = t2.name;
//
type SourceJoin struct {
	*TaskBase
	conf        *RuntimeConfig
	leftStmt    *expr.SqlSource
	rightStmt   *expr.SqlSource
	leftSource  datasource.Scanner
	rightSource datasource.Scanner
}

// A scanner to read from data source
func NewSourceJoin(leftFrom, rightFrom *expr.SqlSource, conf *RuntimeConfig) (*SourceJoin, error) {
	m := &SourceJoin{
		TaskBase: NewTaskBase("SourceJoin"),
	}
	m.TaskBase.TaskType = m.Type()

	m.leftStmt = leftFrom
	m.rightStmt = rightFrom

	u.Debugf("get left1.Name: %v", leftFrom.Name)
	source := conf.Conn(leftFrom.Name)
	u.Debugf("left source: %T", source)
	// Must provider either Scanner, and or Seeker interfaces
	if scanner, ok := source.(datasource.Scanner); !ok {
		u.Errorf("Could not create scanner for %v  %T %#v", leftFrom.Name, source, source)
		return nil, fmt.Errorf("Must Implement Scanner")
	} else {
		m.leftSource = scanner
	}

	u.Debugf("get rightFrom.Name: %v", rightFrom.Name)
	source2 := conf.Conn(rightFrom.Name)
	u.Debugf("source right: %T", source2)
	// Must provider either Scanner, and or Seeker interfaces
	if scanner, ok := source2.(datasource.Scanner); !ok {
		u.Errorf("Could not create scanner for %v  %T %#v", leftFrom.Name, source2, source2)
		return nil, fmt.Errorf("Must Implement Scanner")
	} else {
		m.rightSource = scanner
	}

	return m, nil
}

func (m *SourceJoin) Copy() *Source { return &Source{} }

func (m *SourceJoin) Close() error {
	if closer, ok := m.leftSource.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if closer, ok := m.rightSource.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func joinValue(ctx *Context, node expr.Node, msg datasource.Message) (string, bool) {

	if msg == nil {
		u.Warnf("got nil message?")
	}
	if msgReader, ok := msg.Body().(expr.ContextReader); ok {

		joinVal, ok := vm.Eval(msgReader, node)
		//u.Debugf("msg: %#v", msgReader)
		//u.Infof("evaluating: ok?%v T:%T result=%v node expr:%v", ok, joinVal, joinVal.ToString(), node.StringAST())
		if !ok {
			u.Errorf("could not evaluate: %v", msg)
			return "", false
		}
		switch val := joinVal.(type) {
		case value.StringValue:
			return val.Val(), true
		default:
			u.Warnf("unknown type? %T", joinVal)
		}
	} else {
		u.Errorf("could not convert to message reader: %T", msg.Body())
	}
	return "", false
}

func (m *SourceJoin) Run(context *Context) error {
	defer context.Recover() // Our context can recover panics, save error msg
	defer close(m.msgOutCh) // closing input channels is the signal to stop

	leftIn := m.leftSource.MesgChan(nil)
	rightIn := m.rightSource.MesgChan(nil)

	//u.Warnf("leftSource: %p  rightSource: %p", m.leftSource, m.rightSource)
	//u.Warnf("leftIn: %p  rightIn: %p", leftIn, rightIn)
	outCh := m.MessageOut()

	//u.Infof("Checking leftStmt:  %#v", m.leftStmt)
	//u.Infof("Checking rightStmt:  %#v", m.rightStmt)
	lhExpr, err := m.leftStmt.JoinValueExpr()
	if err != nil {
		return err
	}
	rhExpr, err := m.rightStmt.JoinValueExpr()
	if err != nil {
		return err
	}
	//lcols := m.leftStmt.UnAliasedColumns()
	//rcols := m.rightStmt.UnAliasedColumns()
	cols := m.leftStmt.UnAliasedColumns()
	lh := make(map[string][]datasource.Message)
	rh := make(map[string][]datasource.Message)
	/*
			JOIN = INNER JOIN = Equal Join

			1)   we need to rewrite query for a source based on the Where + Join? + sort needed
			2)

		TODO:
			x get value for join ON to use in hash,  EvalJoinValues(msg) - this is similar to Projection?
			- manage the coordination of draining both/channels
			- evaluate hashes/output
	*/
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		for {
			//u.Infof("In source Scanner iter %#v", item)
			select {
			case <-m.SigChan():
				u.Warnf("got signal quit")
				return
			case msg, ok := <-leftIn:
				if !ok {
					//u.Warnf("NICE, got left shutdown")
					wg.Done()
					return
				} else {
					if jv, ok := joinValue(nil, lhExpr, msg); ok {
						//u.Debugf("left val:%v     %#v", jv, msg.Body())
						lh[jv] = append(lh[jv], msg)
					} else {
						u.Warnf("Could not evaluate? %v", msg.Body())
					}
				}
			}

		}
	}()
	wg.Add(1)
	go func() {
		for {

			//u.Infof("In source Scanner iter %#v", item)
			select {
			case <-m.SigChan():
				u.Warnf("got signal quit")
				return
			case msg, ok := <-rightIn:
				if !ok {
					//u.Warnf("NICE, got right shutdown")
					wg.Done()
					return
				} else {
					if jv, ok := joinValue(nil, rhExpr, msg); ok {
						//u.Debugf("right val:%v     %#v", jv, msg.Body())
						rh[jv] = append(rh[jv], msg)
					} else {
						u.Warnf("Could not evaluate? %v", msg.Body())
					}
				}
			}

		}
	}()
	wg.Wait()
	//u.Info("leaving source scanner")
	i := uint64(0)
	for keyLeft, valLeft := range lh {
		if valRight, ok := rh[keyLeft]; ok {
			//u.Infof("found match?\n\t%d left=%v\n\t%d right=%v", len(valLeft), valLeft, len(valRight), valRight)
			msgs := mergeUvMsgs(valLeft, valRight, cols)
			for _, msg := range msgs {
				outCh <- datasource.NewUrlValuesMsg(i, msg)
			}
		}
	}
	return nil
}

func mergeUvMsgs(lmsgs, rmsgs []datasource.Message, cols map[string]*expr.Column) []*datasource.ContextUrlValues {
	out := make([]*datasource.ContextUrlValues, 0)
	for _, lm := range lmsgs {
		switch lmt := lm.Body().(type) {
		case *datasource.ContextUrlValues:

			for _, rm := range rmsgs {
				switch rmt := rm.Body().(type) {
				case *datasource.ContextUrlValues:
					// for k, val := range rmt.Data {
					// 	u.Debugf("k=%v v=%v", k, val)
					// }
					newMsg := datasource.NewContextUrlValues(url.Values{})
					newMsg = reAlias(newMsg, lmt.Data, cols)
					newMsg = reAlias(newMsg, rmt.Data, cols)
					//u.Debugf("pre:  %#v", lmt.Data)
					//u.Debugf("post:  %#v", newMsg.Data)
					out = append(out, newMsg)
				default:
					u.Warnf("uknown type: %T", rm)
				}
			}
		default:
			u.Warnf("uknown type: %T", lm)
		}
	}
	return out
}

func mergeUv(m1, m2 *datasource.ContextUrlValues) *datasource.ContextUrlValues {
	out := datasource.NewContextUrlValues(m1.Data)
	for k, val := range m2.Data {
		u.Debugf("k=%v v=%v", k, val)
		out.Data[k] = val
	}
	return out
}
func reAlias(m *datasource.ContextUrlValues, vals url.Values, cols map[string]*expr.Column) *datasource.ContextUrlValues {
	for k, val := range vals {
		if col, ok := cols[k]; !ok {
			//u.Warnf("Should not happen? missing %v  ", k)
		} else {
			//u.Infof("found: k=%v as=%v   val=%v", k, col.As, val)
			m.Data[col.As] = val
		}
	}
	return m
}
