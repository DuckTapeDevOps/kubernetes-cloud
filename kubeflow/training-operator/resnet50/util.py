import argparse
import time
from pathlib import Path
from typing import Tuple, Optional

import torch
import torch.nn as nn
import torch.optim as optim
import torch.utils.data.distributed
import wandb
from torch.cuda.amp import GradScaler
from torch.utils.data import DataLoader
from torch.utils.data.distributed import DistributedSampler
from torchvision import transforms, models
from torchvision.datasets import ImageFolder
from torchvision.transforms import transforms
from torchvision.transforms.functional import InterpolationMode


def train_mixed_precision(model: models.resnet50,
                          criterion: nn.CrossEntropyLoss,
                          train_sampler: DistributedSampler,
                          train_loader: DataLoader,
                          optimizer: optim.Optimizer,
                          epoch: int,
                          log_interval: int,
                          use_cuda: bool,
                          scaler: GradScaler,
                          wandb_run: Optional[wandb.run]) -> None:
    model.train()
    # Set epoch to sampler for shuffling.
    train_sampler.set_epoch(epoch)
    for batch_idx, (data, target) in enumerate(train_loader):
        step_start = time.perf_counter()
        if use_cuda:
            data, target = data.cuda(), target.cuda()
        optimizer.zero_grad()
        with torch.cuda.amp.autocast():
            output = model(data)
            loss = criterion(output, target)

        scaler.scale(loss).backward()
        # Make sure all async allreduces are done
        optimizer.synchronize()
        # In-place unscaling of all gradients before weights update
        scaler.unscale_(optimizer)
        with optimizer.skip_synchronize():
            scaler.step(optimizer)
        # Update scaler in case of overflow/underflow
        scaler.update()

        if wandb_run:
            step_time = time.perf_counter() - step_start
            global_step = (epoch - 1) * len(train_loader) + batch_idx
            wandb_info = {"train/loss": loss.item(),
                          "train/epoch": epoch,
                          "train/step": global_step,
                          "train/samples_seen": global_step * len(data),
                          "perf/rank_samples_per_second": len(data) / step_time}
            wandb_run.log(wandb_info, step=global_step)

        if batch_idx % log_interval == 0:
            # Use train_sampler to determine the number of examples in this worker's partition.
            processed_samples = batch_idx * len(data)
            completion_percentage = 100. * batch_idx / len(train_loader)
            print(f'Train Epoch: {epoch} [{processed_samples}/{len(train_sampler)} ({completion_percentage:.0f}%)]'
                  f'\tLoss: {loss.item():.6f}\tLoss Scale: {scaler.get_scale()}')


def train_epoch(model: models.resnet50,
                criterion: nn.CrossEntropyLoss,
                train_sampler: DistributedSampler,
                train_loader: DataLoader,
                optimizer: optim.Optimizer,
                epoch: int,
                log_interval: int,
                use_cuda: bool,
                wandb_run: Optional[wandb.run]) -> None:
    model.train()
    # Set epoch to sampler for shuffling.
    train_sampler.set_epoch(epoch)
    for batch_idx, (data, target) in enumerate(train_loader):
        step_start = time.perf_counter()
        if use_cuda:
            data, target = data.cuda(), target.cuda()
        optimizer.zero_grad()
        output = model(data)
        loss = criterion(output, target)
        loss.backward()
        optimizer.step()

        if wandb_run:
            torch.cuda.synchronize()
            step_time = time.perf_counter() - step_start
            global_step = (epoch - 1) * len(train_loader) + batch_idx
            wandb_info = {"train/loss": loss.item(),
                          "train/epoch": epoch,
                          "train/step": global_step,
                          "train/samples_seen": global_step * len(data),
                          "perf/rank_samples_per_second": len(data) / step_time}
            wandb_run.log(wandb_info, step=global_step)

        if batch_idx % log_interval == 0:
            # Use train_sampler to determine the number of examples in this worker's partition.
            processed_samples = batch_idx * len(data)
            completion_percentage = 100. * batch_idx / len(train_loader)
            print(f'Train Epoch: {epoch} [{processed_samples}/{len(train_sampler)} ({completion_percentage:.0f}%)]'
                  f'\tLoss: {loss.item():.6f}')


def test(model: models.resnet50,
         criterion: nn.CrossEntropyLoss,
         test_sampler: DistributedSampler,
         test_loader: DataLoader,
         use_cuda: bool,
         epoch: int,
         wandb_run: Optional[wandb.run]) -> None:
    model.eval()
    test_loss = 0
    acc1 = 0
    acc5 = 0

    with torch.inference_mode():
        for data, target in test_loader:
            if use_cuda:
                data, target = data.cuda(), target.cuda()
            output = model(data)
            # sum up batch loss
            test_loss += criterion(output, target).item()

            batch_acc1, batch_acc5 = accuracy(output, target)
            acc1 += batch_acc1.item()
            acc5 += batch_acc5.item()

    # Use test_sampler to determine the number of examples in this worker's partition.
    test_loss /= len(test_sampler)
    acc1 /= len(test_sampler)
    acc5 /= len(test_sampler)

    if wandb_run:
        wandb_info = {"test/loss": test_loss,
                      "test/epoch": epoch,
                      "test/acc1": acc1,
                      "test/acc5": acc5}
        wandb_run.log(wandb_info)

    print(f'Test Epoch: {epoch}\tloss={test_loss:.4f}\tAcc@1={acc1:.3f}\tAcc@5={acc5:.3f}')


def accuracy(output, target, topk=(1, 5)):
    """Computes the accuracy over the k top predictions for the specified values of k"""
    with torch.inference_mode():
        maxk = max(topk)
        batch_size = target.size(0)
        if target.ndim == 2:
            target = target.max(dim=1)[1]

        _, pred = output.topk(maxk, 1, True, True)
        pred = pred.t()
        correct = pred.eq(target[None])

        res = []
        for k in topk:
            correct_k = correct[:k].flatten().sum(dtype=torch.float32)
            res.append(correct_k * (100.0 / batch_size))
        return res


def load_data(train_dir: Path,
              test_dir: Path,
              args: argparse.Namespace,
              world_size: int,
              rank: int) -> Tuple[ImageFolder, ImageFolder, DistributedSampler, DistributedSampler]:
    # These are known ImageNet values
    mean = (0.485, 0.456, 0.406)
    std = (0.229, 0.224, 0.225)

    interpolation = InterpolationMode(args.interpolation)
    train_transforms = transforms.Compose([
        transforms.RandomResizedCrop(args.train_crop_size, interpolation=interpolation),
        transforms.PILToTensor(),
        transforms.ConvertImageDtype(torch.float),
        transforms.Normalize(mean=mean, std=std)
    ])
    train_dataset = ImageFolder(str(train_dir), train_transforms)

    test_transforms = transforms.Compose([
        transforms.Resize(args.val_resize_size, interpolation=interpolation),
        transforms.CenterCrop(args.val_crop_size),
        transforms.PILToTensor(),
        transforms.ConvertImageDtype(torch.float),
        transforms.Normalize(mean=mean, std=std),
    ])
    test_dataset = ImageFolder(str(test_dir), test_transforms)

    train_sampler = DistributedSampler(train_dataset, num_replicas=world_size, rank=rank)
    test_sampler = DistributedSampler(test_dataset, num_replicas=world_size, rank=rank)

    return train_dataset, test_dataset, train_sampler, test_sampler
