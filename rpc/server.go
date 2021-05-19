package rpc

import (
	"context"
	"errors"
	"fmt"
	"magnapinna/api"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

type Repository interface {
	StoreLease(context.Context, *api.Lease) error
	FetchLease(context.Context, *api.Registration) (api.Lease, error)
	DeleteLease(context.Context, *api.Lease) error
}

type Server struct {
	api.UnimplementedMagnapinnaServer
	srv        *grpc.Server
	ctx        context.Context
	timeout    time.Duration
	conns      ConnCache
	repository Repository
	observer   observer
}

type ConnCache struct {
	mut    *sync.Mutex
	active map[string]api.Magnapinna_JoinClusterServer
}

type RepositoryError struct {
	s string
}

const repoErr = "error communicating with repository: "

func (r RepositoryError) Error() string {
	return fmt.Sprintf("%s%s", repoErr, r.s)
}

func (r RepositoryError) Sanitized() string {
	return repoErr
}

var ErrNoLease = errors.New("no lease matching identifier found")

func (s *Server) CheckRegistration(ctx context.Context, rs *api.Registration) (*api.Lease, error) {
	lease, err := s.repository.FetchLease(ctx, rs)
	if err == ErrNoLease || errors.As(err, &RepositoryError{}) {
		return nil, err
	}
	return &lease, nil
}

func (s *Server) Register(ctx context.Context, rs *api.Registration) (*api.Lease, error) {
	lease, err := s.repository.FetchLease(ctx, rs)
	if err != nil && err != ErrNoLease {
		return nil, err
	}
	lease.Expiration = time.Now().Unix() + int64(rs.Duration)
	lease.Identifier = rs.Identifier
	err = s.repository.StoreLease(ctx, &lease)
	return &lease, err
}

func (s *Server) Deregister(ctx context.Context, rs *api.Registration) (*api.Lease, error) {
	lease, err := s.repository.FetchLease(ctx, rs)
	if err != nil && err != ErrNoLease {
		return nil, err
	} else if err == ErrNoLease {
		return &api.Lease{}, nil
	}
	err = s.repository.DeleteLease(ctx, &lease)
	return &lease, err
}

func (s *Server) JoinCluster(join api.Magnapinna_JoinClusterServer) error {
	init, err := join.Recv()
	if err != nil {
		s.observer.ObserveJoinFailure(err)
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	_, err = s.CheckRegistration(ctx, &api.Registration{
		Identifier: init.Identifier,
	})
	cancel()
	if err != nil {
		return err
	}

	// Hand off JoinClusterServer to be used in StartSession calls, then await
	// context cancellation prior to setting Trailer and returning.
	err = s.conns.addClient(init.Identifier, join)
	if err != nil {
		return err
	}
	done := join.Context().Done()
	<-done
	join.SetTrailer(metadata.New(map[string]string{"closed": "true"}))
	return nil
}

func (s *Server) StartSession(sess api.Magnapinna_StartSessionServer) error {
	init, err := sess.Recv()
	if err != nil {
		s.observer.ObserveJoinFailure(err)
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	_, err = s.CheckRegistration(ctx, &api.Registration{
		Identifier: init.Identifier,
	})
	cancel()
	if err != nil {
		return err
	}

	remote, err := s.conns.getClient(init.Identifier)
	if err != nil {
		return err
	}
	done := s.ctx.Done()

	for {
		select {
		case <-done:
			sess.SetTrailer(metadata.New(map[string]string{"closed": "true"}))
			return nil
		default:
			// TODO handle back off if we're erroring sequentially
			cmd, err := sess.Recv()
			if err != nil {
				s.observer.ObserveCommandFailure(err)
			}
			err = remote.Send(cmd)
			if err != nil {
				s.observer.ObserveJoinFailure(err)
			}
		}
	}
	return nil
}

func (c *ConnCache) addClient(id string, join api.Magnapinna_JoinClusterServer) error {
	c.mut.Lock()
	defer c.mut.Unlock()
	_, found := c.active[id]
	if found {
		return fmt.Errorf("ID %s has already connected", id)
	}
	c.active[id] = join
	return nil
}

func (c *ConnCache) getClient(id string) (api.Magnapinna_JoinClusterServer, error) {
	c.mut.Lock()
	defer c.mut.Unlock()
	client, found := c.active[id]
	if !found {
		return nil, fmt.Errorf("no action client with ID %s", id)
	}
	return client, nil
}
