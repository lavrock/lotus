package processor

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"

	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/lotus/chain/events/state"
	"github.com/filecoin-project/specs-actors/actors/builtin"
)

func (p *Processor) setupCommonActors() error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`
create table if not exists id_address_map
(
	id text not null,
	address text not null,
	constraint id_address_map_pk
		primary key (id, address)
);

create unique index if not exists id_address_map_id_uindex
	on id_address_map (id);

create unique index if not exists id_address_map_address_uindex
	on id_address_map (address);

create table if not exists actors
  (
	id text not null
		constraint id_address_map_actors_id_fk
			references id_address_map (id),
	code text not null,
	head text not null,
	nonce int not null,
	balance text not null,
	stateroot text
  );
  
create index if not exists actors_id_index
	on actors (id);

create index if not exists id_address_map_address_index
	on id_address_map (address);

create index if not exists id_address_map_id_index
	on id_address_map (id);

create or replace function actor_tips(epoch bigint)
    returns table (id text,
                    code text,
                    head text,
                    nonce int,
                    balance text,
                    stateroot text,
                    height bigint,
                    parentstateroot text) as
$body$
    select distinct on (id) * from actors
        inner join state_heights sh on sh.parentstateroot = stateroot
        where height < $1
		order by id, height desc;
$body$ language sql;

create table if not exists actor_states
(
	head text not null,
	code text not null,
	state json not null
);

create unique index if not exists actor_states_head_code_uindex
	on actor_states (head, code);

create index if not exists actor_states_head_index
	on actor_states (head);

create index if not exists actor_states_code_head_index
	on actor_states (head, code);

`); err != nil {
		return err
	}

	return tx.Commit()
}

func (p *Processor) HandleCommonActorsChanges(ctx context.Context, actors map[cid.Cid]ActorTips) error {
	if err := p.storeActorAddresses(ctx, actors); err != nil {
		return err
	}

	grp, _ := errgroup.WithContext(ctx)

	grp.Go(func() error {
		if err := p.storeActorHeads(actors); err != nil {
			return err
		}
		return nil
	})

	grp.Go(func() error {
		if err := p.storeActorStates(actors); err != nil {
			return err
		}
		return nil
	})

	return grp.Wait()
}

type UpdateAddresses struct {
	Old state.AddressPair
	New state.AddressPair
}

func (p Processor) storeActorAddresses(ctx context.Context, actors map[cid.Cid]ActorTips) error {
	start := time.Now()
	defer func() {
		log.Debugw("Stored Actor Addresses", "duration", time.Since(start).String())
	}()

	addressToID := map[address.Address]address.Address{}
	// HACK until genesis storage is figured out:
	addressToID[builtin.SystemActorAddr] = builtin.SystemActorAddr
	addressToID[builtin.InitActorAddr] = builtin.InitActorAddr
	addressToID[builtin.RewardActorAddr] = builtin.RewardActorAddr
	addressToID[builtin.CronActorAddr] = builtin.CronActorAddr
	addressToID[builtin.StoragePowerActorAddr] = builtin.StoragePowerActorAddr
	addressToID[builtin.StorageMarketActorAddr] = builtin.StorageMarketActorAddr
	addressToID[builtin.VerifiedRegistryActorAddr] = builtin.VerifiedRegistryActorAddr
	addressToID[builtin.BurntFundsActorAddr] = builtin.BurntFundsActorAddr

	addressesToUpdate := []UpdateAddresses{}

	pred := state.NewStatePredicates(p.node)
	for _, act := range actors[builtin.InitActorCodeID] {
		for _, info := range act {
			changed, val, err := pred.OnInitActorChange(pred.OnAddressMapChange())(ctx, info.parentTsKey, info.tsKey)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			changes := val.(*state.InitActorAddressChanges)
			for _, add := range changes.Added {
				addressToID[add.PK] = add.ID
			}
			// we'll need to update any addresses that were modified, this indicates a reorg.
			for _, mod := range changes.Modified {
				addressesToUpdate = append(addressesToUpdate, UpdateAddresses{
					Old: mod.From,
					New: mod.To,
				})
			}
		}
	}

	updateTx, err := p.db.Begin()
	if err != nil {
		return err
	}

	for _, updates := range addressesToUpdate {
		if _, err := updateTx.Exec(
			fmt.Sprintf("update id_address_map set id=%s, address=%s where id=%s and address=%s", updates.New.ID, updates.New.PK, updates.Old.ID, updates.Old.PK),
		); err != nil {
			return err
		}
	}
	if err := updateTx.Commit(); err != nil {
		return err
	}

	tx, err := p.db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`
create temp table iam (like id_address_map excluding constraints) on commit drop;
`); err != nil {
		return xerrors.Errorf("prep temp: %w", err)
	}

	stmt, err := tx.Prepare(`copy iam (id, address) from STDIN `)
	if err != nil {
		return err
	}

	for a, i := range addressToID {
		if i == address.Undef {
			continue
		}
		if _, err := stmt.Exec(
			i.String(),
			a.String(),
		); err != nil {
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(`insert into id_address_map select * from iam on conflict (id) do nothing`); err != nil {
		return xerrors.Errorf("actor put: %w", err)
	}

	return tx.Commit()
}

func (p *Processor) storeActorHeads(actors map[cid.Cid]ActorTips) error {
	start := time.Now()
	defer func() {
		log.Debugw("Stored Actor Heads", "duration", time.Since(start).String())
	}()
	// Basic
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		create temp table a (like actors excluding constraints) on commit drop;
	`); err != nil {
		return xerrors.Errorf("prep temp: %w", err)
	}

	stmt, err := tx.Prepare(`copy a (id, code, head, nonce, balance, stateroot) from stdin `)
	if err != nil {
		return err
	}

	for code, actTips := range actors {
		for _, actorInfo := range actTips {
			for _, a := range actorInfo {
				if _, err := stmt.Exec(a.addr.String(), code.String(), a.act.Head.String(), a.act.Nonce, a.act.Balance.String(), a.stateroot.String()); err != nil {
					return err
				}
			}
		}
	}

	if err := stmt.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(`insert into actors select * from a on conflict do nothing `); err != nil {
		return xerrors.Errorf("actor put: %w", err)
	}

	return tx.Commit()
}

func (p *Processor) storeActorStates(actors map[cid.Cid]ActorTips) error {
	start := time.Now()
	defer func() {
		log.Debugw("Stored Actor States", "duration", time.Since(start).String())
	}()
	// States
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		create temp table a (like actor_states excluding constraints) on commit drop;
	`); err != nil {
		return xerrors.Errorf("prep temp: %w", err)
	}

	stmt, err := tx.Prepare(`copy a (head, code, state) from stdin `)
	if err != nil {
		return err
	}

	for code, actTips := range actors {
		for _, actorInfo := range actTips {
			for _, a := range actorInfo {
				if _, err := stmt.Exec(a.act.Head.String(), code.String(), a.state); err != nil {
					return err
				}
			}
		}
	}

	if err := stmt.Close(); err != nil {
		return err
	}

	if _, err := tx.Exec(`insert into actor_states select * from a on conflict do nothing `); err != nil {
		return xerrors.Errorf("actor put: %w", err)
	}

	return tx.Commit()
}
